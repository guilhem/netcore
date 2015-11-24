package netdns

import (
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dustywilson/dnscache"
	"github.com/miekg/dns"
)

// Service provides netcore DNS services.
type Service struct {
	instance string
	cfg      Config
	p        Provider
	cache    *dnscache.Cache
	started  chan bool
	done     chan Completion
}

// NewService creates a new netcore DNS service.
func NewService(p Provider, instance string) *Service {
	s := &Service{
		instance: instance,
		p:        p,
		started:  make(chan bool, 1),
		done:     make(chan Completion, 1),
	}

	go s.init()

	return s
}

func (s *Service) init() {
	if err := s.loadConfig(); err != nil {
		s.signalStarted(false)
		s.signalDone(false, err)
		return
	}

	cacheRetentionNotFound := time.Second * 30 // FIXME: Get rid of this once dnscache is updated to follow spec
	s.cache = dnscache.New(dnsCacheBufferSize, s.cfg.CacheRetention(), cacheRetentionNotFound, func(c dnscache.Context, q dns.Question) []dns.RR {
		return s.answerQuestion(c, &q, 0)
	})

	dns.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) { s.dnsQueryServe(w, req) })

	s.signalStarted(true)

	once := makeOnce() // Avoid calling signalDone twice

	go func() {
		err := dns.ListenAndServe(s.cfg.NetworkAddress(), "tcp", nil)
		if _, ok := <-once; ok {
			s.signalDone(true, err)
		}
	}()

	go func() {
		err := dns.ListenAndServe(s.cfg.NetworkAddress(), "udp", nil)
		if _, ok := <-once; ok {
			s.signalDone(true, err)
		}
	}()
}

func (s *Service) signalStarted(success bool) {
	s.started <- success
	close(s.started)
}

func (s *Service) signalDone(initialized bool, err error) {
	s.done <- Completion{false, err}
	close(s.done)
}

func (s *Service) loadConfig() error {
	// FIXME: Don't make this Init() call, but instead handle initial setup via the CLI
	if err := s.p.Init(); err != nil {
		return err
	}
	cfg, err := s.p.Config(s.instance)
	if err != nil {
		return err
	}
	if err := Validate(cfg); err != nil {
		return err
	}
	s.cfg = cfg
	return nil
}

// Started returns a channel that will be signaled when the service has started
// or failed to start. If the returned value is true the service started
// succesfully.
func (s *Service) Started() chan bool {
	return s.started
}

// Done returns a channel that will be signaled when the service exits.
func (s *Service) Done() chan Completion {
	return s.done
}

func (s *Service) dnsQueryServe(w dns.ResponseWriter, req *dns.Msg) {
	start := time.Now()

	if req.MsgHdr.Response == true { // supposed responses sent to us are bogus
		q := req.Question[0]
		log.Printf("DNS Query IS BOGUS %s %s from %s.\n", q.Name, dns.Type(q.Qtype).String(), w.RemoteAddr())
		return
	}

	// TODO: handle AXFR/IXFR (full and incremental) *someday* for use by non-netcore slaves
	//       ... also if we do that, also handle sending NOTIFY to listed slaves attached to the SOA record

	// Process questions in parallel
	pending := make([]chan []dns.RR, 0, len(req.Question)) // Slice of answer channels
	for i := range req.Question {
		q := &req.Question[i]
		log.Printf("DNS Query [%d/%d] %s %s from %s\n", i+1, len(req.Question), q.Name, dns.Type(q.Qtype).String(), w.RemoteAddr())
		pending = append(pending, s.serveQuestion(q, start))
	}

	// Assemble answers according to the order of the questions
	var answers []dns.RR
	for _, ch := range pending {
		answers = append(answers, <-ch...)
	}

	for _, answer := range answers {
		log.Printf("  [%9.04fms] ANSWER  %s\n", msElapsed(start, time.Now()), answer.String())
	}

	if len(answers) > 0 {
		//log.Printf("OUR DATA: [%+v]\n", answerMsg)
		answerMsg := prepareAnswerMsg(req, answers)
		w.WriteMsg(answerMsg)
		return
	}

	//log.Printf("NO DATA: [%+v]\n", answerMsg)

	failMsg := prepareFailureMsg(req)
	w.WriteMsg(failMsg)
}

func (s *Service) serveQuestion(q *dns.Question, start time.Time) chan []dns.RR {
	output := make(chan []dns.RR)
	var answers []dns.RR

	// is this a WOL query?
	// FIXME: Find a suitable design for this in light of the refactor
	/*
		if isWOLTrigger(q) {
			answer := processWOL(cfg, q)
			answers = append(answers, answer)
		}
	*/

	rc := make(chan []dns.RR)

	s.cache.Lookup(dnscache.Request{
		Question:     *q,
		Start:        start,
		ResponseChan: rc,
	})

	go func() {
		answers = append(answers, <-rc...)
		output <- answers
	}()

	return output
}

func (s *Service) answerQuestion(c dnscache.Context, q *dns.Question, qDepth uint32) []dns.RR {
	if c.Event == dnscache.Renewal && qDepth == 0 {
		log.Printf("DNS Renewal     %s %s\n", q.Name, dns.Type(q.Qtype).String())
	} else {
		log.Printf("  [%9.04fms] %-7s %s %s\n", msElapsed(c.Start, time.Now()), strings.ToUpper(c.Event.String()), q.Name, dns.Type(q.Qtype).String())
	}
	answerTTL := uint32(s.cfg.DefaultTTL() / time.Second)
	var answers []dns.RR
	var secondaryAnswers []dns.RR
	var wouldLikeForwarder = true

	entry, rrType, err := s.fetchBestEntry(q)

	if err == nil {
		wouldLikeForwarder = false
		if entry.TTL > 0 {
			answerTTL = entry.TTL
		}
		log.Printf("  [%9.04fms] FOUND   %s %s\n", msElapsed(c.Start, time.Now()), q.Name, dns.Type(rrType).String())

		switch q.Qtype {
		case dns.TypeSOA:
			answer := answerSOA(q, entry)
			answers = append(answers, answer)
		default:
			// ... for answers that have values
			for i := range entry.Values {
				value := &entry.Values[i]
				if value.Expiration != nil {
					expiration := value.Expiration.Unix()
					now := time.Now().Unix()
					if expiration < now {
						//log.Printf("[Lookup [%s] [%s] (is expired)]\n", q.Name, qType)
						continue
					}
					remaining := uint32(expiration - now)
					if remaining < answerTTL {
						answerTTL = remaining
						log.Printf("  [%9.04fms] EXPIRES %d\n", msElapsed(c.Start, time.Now()), remaining)
					}
				}
				if value.TTL > 0 && value.TTL < answerTTL {
					answerTTL = value.TTL
				}
				switch rrType {
				// FIXME: Add more RR types!
				//        http://godoc.org/github.com/miekg/dns has info as well as
				//        http://en.wikipedia.org/wiki/List_of_DNS_record_types
				case dns.TypeTXT:
					answer := answerTXT(q, value)
					answers = append(answers, answer)
				case dns.TypeA:
					answer := answerA(q, value)
					answers = append(answers, answer)
				case dns.TypeAAAA:
					answer := answerAAAA(q, value)
					answers = append(answers, answer)
				case dns.TypeNS:
					answer := answerNS(q, value)
					answers = append(answers, answer)
				case dns.TypeCNAME:
					answer, target := answerCNAME(q, value)
					answers = append(answers, answer)
					q2 := q
					q2.Name = target // replace question's name with new name
					secondaryAnswers = append(secondaryAnswers, s.answerQuestion(c, q2, qDepth+1)...)
				case dns.TypeDNAME:
					answer := answerDNAME(q, value)
					answers = append(answers, answer)
					wouldLikeForwarder = true
				case dns.TypePTR:
					answer := answerPTR(q, value)
					answers = append(answers, answer)
				case dns.TypeMX:
					answer := answerMX(q, value)
					// FIXME: are we supposed to be returning these in prio ordering?
					//        ... or maybe it does that for us?  or maybe it's the enduser's problem?
					answers = append(answers, answer)
				case dns.TypeSRV:
					answer := answerSRV(q, value)
					// FIXME: are we supposed to be returning these rando-weighted and in priority ordering?
					//        ... or maybe it does that for us?  or maybe it's the enduser's problem?
					answers = append(answers, answer)
				case dns.TypeSSHFP:
					// TODO: implement SSHFP
					//       http://godoc.org/github.com/miekg/dns#SSHFP
					//       NOTE: we must implement DNSSEC before using this RR type
				}
			}
		}
	}

	for _, answer := range answers {
		answer.Header().Ttl = answerTTL // FIXME: I think this might be inappropriate
		//log.Printf("[APPLIED TTL [%s] [%s] %d]\n", q.Name, dns.Type(q.Qtype).String(), answerTTL)
	}

	// Append the results of secondary queries, such as the results of CNAME and DNAME records
	answers = append(answers, secondaryAnswers...)

	// check to see if we host this zone; if yes, don't allow use of ext forwarders
	// ... also, check to see if we hit a DNAME so we can handle that aliasing
	// FIXME: Only forward if we are configured as a forwarder
	if wouldLikeForwarder && !s.haveAuthority(q) {
		log.Printf("  [%9.04fms] FORWARD %s %s\n", msElapsed(c.Start, time.Now()), q.Name, dns.Type(q.Qtype).String())
		answers = append(answers, forwardQuestion(q, s.cfg.Forwarders())...)
	}

	// FIXME: Check whether a default TTL can and should be applied to unanswered queries

	return answers
}

// fetchBestEntry will return the most suitable entry from the DNS database for
// the given query. If no suitable entry is found it will return ErrNotFound.
func (s *Service) fetchBestEntry(q *dns.Question) (entry *DNSEntry, rrType uint16, err error) {
	err = ErrNotFound
	for _, result := range s.fetchRelatedEntries(q) {
		data := <-result
		entry, rrType, err = data.Entry, data.RType, data.Err
		if err == nil {
			return
		}
		// FIXME: Test for missing entries specifically, not just any error
	}
	return
}

// fetchRelatedEntries issues parallel queries to the DNS database for all
// records possibly needed to answer the given question, and returns a slice of
// channels from which to retrieve answers in prioritized order.
func (s *Service) fetchRelatedEntries(q *dns.Question) []chan dnsEntryResult {
	// Issue the CNAME and RR queries simultaneously
	entries := make([]chan dnsEntryResult, 0, 2)
	entries = append(entries, s.fetchEntry(q, dns.TypeCNAME))
	if q.Qtype != dns.TypeCNAME {
		entries = append(entries, s.fetchEntry(q, q.Qtype))
	}
	if q.Qtype != dns.TypeDNAME {
		// TODO: Check for DNAME entries for the given name and for each parent for
		//       which we have authority.
		//queries = append(queries, fetchEntry(cfg, q, dns.TypeDNAME))
	}
	return entries
}

func (s *Service) fetchEntry(q *dns.Question, rrType uint16) chan dnsEntryResult {
	out := make(chan dnsEntryResult)
	go func() {
		entry, err := s.p.RR(q.Name, dns.Type(rrType).String())
		out <- dnsEntryResult{
			Entry: entry,
			RType: rrType,
			Err:   err,
		}
	}()
	return out
}

// haveAuthority returns true if we are an authority for the zone containing
// the given key
func (s *Service) haveAuthority(q *dns.Question) bool {
	nameParts := strings.Split(strings.TrimSuffix(q.Name, "."), ".") // breakup the queryed name
	// Check for authority at each level (but ignore the TLD)
	for i := 0; i < len(nameParts)-1; i++ {
		name := strings.Join(nameParts[i:], ".")
		// Test for an SOA (which tells us we have authority)
		found, err := s.p.HasRR(name, "SOA")
		if err == nil && found {
			return true
		}
		// Test for a DNAME which has special handling for aliasing of subdomains within
		found, err = s.p.HasRR(name, "DNAME")
		if err == nil && found {
			// FIXME!  THIS NEEDS TO HANDLE DNAME ALIASING CORRECTLY INSTEAD OF IGNORING IT...
			log.Printf("DNAME EXISTS!  WE NEED TO HANDLE THIS CORRECTLY... FIXME\n")
			return true
		}
	}
	return false
}

// msElapsed returns the number of milliseconds that have elapsed between now
// and start as a float64
func msElapsed(start, now time.Time) float64 {
	elapsed := now.Sub(start)
	seconds := elapsed.Seconds()
	return seconds * 1000
}

func prepareAnswerMsg(req *dns.Msg, answers []dns.RR) *dns.Msg {
	answerMsg := new(dns.Msg)
	answerMsg.Id = req.Id
	answerMsg.Response = true
	answerMsg.Authoritative = true
	answerMsg.Question = req.Question
	answerMsg.Answer = answers
	answerMsg.Rcode = dns.RcodeSuccess
	answerMsg.Extra = []dns.RR{}
	return answerMsg
}

func prepareFailureMsg(req *dns.Msg) *dns.Msg {
	failMsg := new(dns.Msg)
	failMsg.Id = req.Id
	failMsg.Response = true
	failMsg.Authoritative = true
	failMsg.Question = req.Question
	failMsg.Rcode = dns.RcodeNameError
	return failMsg
}

func isWOLTrigger(q *dns.Question) bool {
	wolMatcher := regexp.MustCompile(`^_wol\.`)
	return q.Qclass == dns.ClassINET && q.Qtype == dns.TypeTXT && wolMatcher.MatchString(q.Name)
}

func getWOLHostname(q *dns.Question) string {
	wolMatcher := regexp.MustCompile(`^_wol\.`)
	return wolMatcher.ReplaceAllString(q.Name, "")
}

// FIXME: Restore WOL functionality
/*
func processWOL(cfg *Config, q *dns.Question) dns.RR {
	hostname := getWOLHostname(q)
	log.Printf("WoL requested for %s", hostname)
	err := wakeByHostname(cfg, hostname)
	status := "OKAY"
	if err != nil {
		status = err.Error()
	}
	answer := new(dns.TXT)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeTXT
	answer.Header().Class = dns.ClassINET
	answer.Txt = []string{status}
	return answer
}
*/

func answerSOA(q *dns.Question, e *DNSEntry) dns.RR {
	answer := new(dns.SOA)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeSOA
	answer.Header().Class = dns.ClassINET
	answer.Ns = strings.TrimSuffix(e.Meta["ns"], ".") + "."
	answer.Mbox = strings.TrimSuffix(e.Meta["mbox"], ".") + "."
	answer.Serial = uint32(time.Now().Unix())
	answer.Refresh = uint32(60) // only used for master->slave timing
	answer.Retry = uint32(60)   // only used for master->slave timing
	answer.Expire = uint32(60)  // only used for master->slave timing
	answer.Minttl = uint32(60)  // how long caching resolvers should cache a miss (NXDOMAIN status)
	return answer
}

func answerTXT(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.TXT)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeTXT
	answer.Header().Class = dns.ClassINET
	answer.Txt = []string{v.Value}
	return answer
}

func answerA(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.A)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeA
	answer.Header().Class = dns.ClassINET
	answer.A = net.ParseIP(v.Value)
	return answer
}

func answerAAAA(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.AAAA)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeAAAA
	answer.Header().Class = dns.ClassINET
	answer.AAAA = net.ParseIP(v.Value)
	return answer
}

func answerNS(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.NS)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeNS
	answer.Header().Class = dns.ClassINET
	answer.Ns = strings.TrimSuffix(v.Value, ".") + "."
	return answer
}

func answerCNAME(q *dns.Question, v *DNSValue) (dns.RR, string) {
	// Info: http://en.wikipedia.org/wiki/CNAME_record
	answer := new(dns.CNAME)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeCNAME
	answer.Header().Class = dns.ClassINET
	answer.Target = strings.TrimSuffix(v.Value, ".") + "."
	return answer, answer.Target
}

func answerDNAME(q *dns.Question, v *DNSValue) dns.RR {
	// FIXME: This is not being used correctly.  See the notes about
	//        fixing CNAME and then consider that DNAME takes it a
	//        big step forward and aliases an entire subtree, not just
	//        a single name in the tree.  Note that this is for pointing
	//        to subtree, not to the self-equivalent.  See the Wikipedia
	//        article about it, linked below.  See also CNAME above.
	//				... http://en.wikipedia.org/wiki/CNAME_record#DNAME_record
	answer := new(dns.DNAME)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeDNAME
	answer.Header().Class = dns.ClassINET
	answer.Target = strings.TrimSuffix(v.Value, ".") + "."
	return answer
}

func answerPTR(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.PTR)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypePTR
	answer.Header().Class = dns.ClassINET
	answer.Ptr = strings.TrimSuffix(v.Value, ".") + "."
	return answer
}

func answerMX(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.MX)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeMX
	answer.Header().Class = dns.ClassINET
	answer.Preference = 50 // default if not defined
	priority, err := strconv.Atoi(v.Attr["priority"])
	if err == nil {
		answer.Preference = uint16(priority)
	}
	if target, ok := v.Attr["target"]; ok {
		answer.Mx = strings.TrimSuffix(target, ".") + "."
	} else if v.Value != "" { // allows for simplified setting
		answer.Mx = strings.TrimSuffix(v.Value, ".") + "."
	}
	return answer
}

func answerSRV(q *dns.Question, v *DNSValue) dns.RR {
	answer := new(dns.SRV)
	answer.Header().Name = q.Name
	answer.Header().Rrtype = dns.TypeSRV
	answer.Header().Class = dns.ClassINET
	answer.Priority = 50 // default if not defined
	priority, err := strconv.Atoi(v.Attr["priority"])
	if err == nil {
		answer.Priority = uint16(priority)
	}
	answer.Weight = 50 // default if not defined
	weight, err := strconv.Atoi(v.Attr["weight"])
	if err == nil {
		answer.Weight = uint16(weight)
	}
	answer.Port = 0 // default if not defined
	port, err := strconv.Atoi(v.Attr["port"])
	if err == nil {
		answer.Port = uint16(port)
	}
	if target, ok := v.Attr["target"]; ok {
		answer.Target = strings.TrimSuffix(target, ".") + "."
	} else if v.Value != "" { // allows for simplified setting
		targetParts := strings.Split(v.Value, ":")
		answer.Target = strings.TrimSuffix(targetParts[0], ".") + "."
		if len(targetParts) > 1 {
			port, err := strconv.Atoi(targetParts[1])
			if err == nil {
				answer.Port = uint16(port)
			}
		}
	}
	return answer
}

func forwardQuestion(q *dns.Question, forwarders []string) []dns.RR {
	//qType := dns.Type(q.Qtype).String() // query type
	//log.Printf("[Forwarder Lookup [%s] [%s]]\n", q.Name, qType)

	myReq := new(dns.Msg)
	myReq.SetQuestion(q.Name, q.Qtype)

	if len(forwarders) == 0 {
		// we have no upstreams, so we'll just not use any
	} else if strings.TrimSpace(forwarders[0]) == "!" {
		// we've been told explicitly to not pass anything along to any upsteams
	} else {
		c := new(dns.Client)
		for _, server := range forwarders {
			c.Net = "udp"
			m, _, err := c.Exchange(myReq, strings.TrimSpace(server))

			if m != nil && m.MsgHdr.Truncated {
				c.Net = "tcp"
				m, _, err = c.Exchange(myReq, strings.TrimSpace(server))
			}

			// FIXME: Cache misses.  And cache hits, too.

			if err != nil {
				//log.Printf("[Forwarder Lookup [%s] [%s] failed: [%s]]\n", q.Name, qType, err)
				log.Println(err)
			} else {
				//log.Printf("[Forwarder Lookup [%s] [%s] success]\n", q.Name, qType)
				return m.Answer
			}
		}
	}
	return nil
}

// FIXME: please support DNSSEC, verification, signing, etc...