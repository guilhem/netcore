package main

import (
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

type DNSDB interface {
	InitDNS()
	GetDNS(name string, rtype string) (*DNSEntry, error)
	HasDNS(name string, rtype string) (bool, error)
	RegisterA(fqdn string, ip net.IP, exclusive bool, ttl uint32, expiration uint64) error
}

type DNSEntry struct {
	TTL    uint32
	Values []DNSValue
	Meta   map[string]string
}

type DNSValue struct {
	Expiration *time.Time
	TTL        uint32
	Value      string
	Attr       map[string]string
}

func dnsSetup(cfg *Config) chan error {
	log.Println("DNSSETUP")

	dns.HandleFunc(".", func(w dns.ResponseWriter, req *dns.Msg) { dnsQueryServe(cfg, w, req) })
	cfg.db.InitDNS()
	exit := make(chan error, 1)

	go func() {
		exit <- dns.ListenAndServe("0.0.0.0:53", "tcp", nil) // TODO: should use cfg to define the listening ip/port
	}()

	go func() {
		exit <- dns.ListenAndServe("0.0.0.0:53", "udp", nil) // TODO: should use cfg to define the listening ip/port
	}()

	return exit
}

func dnsQueryServe(cfg *Config, w dns.ResponseWriter, req *dns.Msg) {
	if req.MsgHdr.Response == true { // supposed responses sent to us are bogus
		q := req.Question[0]
		log.Printf("DNS Query IS BOGUS %s %s from %s.\n", q.Name, dns.Type(q.Qtype).String(), w.RemoteAddr())
		return
	}

	// TODO: handle AXFR/IXFR (full and incremental) *someday* for use by non-netcore slaves
	//       ... also if we do that, also handle sending NOTIFY to listed slaves attached to the SOA record

	// FIXME: Make the default TTL into a configuration parameter
	defaultTTL := uint32(10800) // this is the default TTL = 3 hours

	var answers []dns.RR

	for i := range req.Question {
		q := &req.Question[i]
		log.Printf("DNS Query [%d/%d] %s %s from %s.\n", i+1, len(req.Question), q.Name, dns.Type(q.Qtype).String(), w.RemoteAddr())
		// TODO: lookup in an in-memory cache (obeying TTLs!)
		answers = append(answers, answerQuestion(cfg, q, defaultTTL)...)
		// TODO: Cache the response locally in RAM?
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

func answerQuestion(cfg *Config, q *dns.Question, defaultTTL uint32) []dns.RR {
	answerTTL := defaultTTL
	var answers []dns.RR
	var secondaryAnswers []dns.RR

	// is this a WOL query?
	if isWOLTrigger(q) {
		answer := processWOL(cfg, q)
		answers = append(answers, answer)
	}

	qType := dns.Type(q.Qtype).String() // query type
	log.Printf("[Lookup [%s] [%s] %d]\n", q.Name, qType, answerTTL)

	var wouldLikeForwarder = true

	// TODO: Issue the CName and RR etcd queries simultaneously

	// Always attempt CNAME lookup first
	entry, err := cfg.db.GetDNS(q.Name, "CNAME")

	// Look up the requested RR type
	if err != nil { // FIXME: Test for missing entries specifically, not just any error
		//log.Printf("[Lookup [%s] [%s] (normal lookup) %s]\n", q.Name, qType, key)
		entry, err = cfg.db.GetDNS(q.Name, qType)
	}

	if err == nil {
		//log.Printf("[Lookup [%s] [%s] (matched something)]\n", q.Name, qType)
		wouldLikeForwarder = false

		if entry.TTL > 0 {
			answerTTL = entry.TTL
			log.Printf("[FOUND TTL [%s] [%s] %d]\n", q.Name, dns.Type(q.Qtype).String(), answerTTL)
		}

		switch qType {
		case "SOA":
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
						log.Printf("[TTL-BY-EXPIRATION [%s] [%s] %d]\n", q.Name, dns.Type(q.Qtype).String(), answerTTL)
					}
				}
				if value.TTL > 0 && value.TTL < answerTTL {
					answerTTL = value.TTL
				}
				switch qType {
				// FIXME: Add more RR types!
				//        http://godoc.org/github.com/miekg/dns has info as well as
				//        http://en.wikipedia.org/wiki/List_of_DNS_record_types
				case "TXT":
					answer := answerTXT(q, value)
					answers = append(answers, answer)
				case "A":
					answer := answerA(q, value)
					answers = append(answers, answer)
				case "AAAA":
					answer := answerAAAA(q, value)
					answers = append(answers, answer)
				case "NS":
					answer := answerNS(q, value)
					answers = append(answers, answer)
				case "CNAME":
					answer, target := answerCNAME(q, value)
					answers = append(answers, answer)
					q2 := q
					q2.Name = target // replace question's name with new name
					secondaryAnswers = append(secondaryAnswers, answerQuestion(cfg, q2, defaultTTL)...)
				case "DNAME":
					answer := answerDNAME(q, value)
					answers = append(answers, answer)
					wouldLikeForwarder = true
				case "PTR":
					answer := answerPTR(q, value)
					answers = append(answers, answer)
				case "MX":
					answer := answerMX(q, value)
					// FIXME: are we supposed to be returning these in prio ordering?
					//        ... or maybe it does that for us?  or maybe it's the enduser's problem?
					answers = append(answers, answer)
				case "SRV":
					answer := answerSRV(q, value)
					// FIXME: are we supposed to be returning these rando-weighted and in priority ordering?
					//        ... or maybe it does that for us?  or maybe it's the enduser's problem?
					answers = append(answers, answer)
				case "SSHFP":
					// TODO: implement SSHFP
					//       http://godoc.org/github.com/miekg/dns#SSHFP
					//       NOTE: we must implement DNSSEC before using this RR type
				}
			}
		}
	}

	log.Printf("[APPLIED TTL [%s] [%s] %d]\n", q.Name, dns.Type(q.Qtype).String(), answerTTL)
	for _, answer := range answers {
		answer.Header().Ttl = answerTTL // FIXME: I think this might be inappropriate
	}

	// Append the results of secondary queries, such as the results of CNAME and DNAME records
	answers = append(answers, secondaryAnswers...)

	// check to see if we host this zone; if yes, don't allow use of ext forwarders
	// ... also, check to see if we hit a DNAME so we can handle that aliasing
	// FIXME: Only forward if we are configured as a forwarder
	if wouldLikeForwarder && !haveAuthority(cfg, q) {
		answers = append(answers, forwardQuestion(q, cfg.DNSForwarders())...)
	}

	return answers
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

// haveAuthority returns true if we are an authority for the zone containing
// the given key
func haveAuthority(cfg *Config, q *dns.Question) bool {
	nameParts := strings.Split(strings.TrimSuffix(q.Name, "."), ".") // breakup the queryed name
	// Check for authority at each level (but ignore the TLD)
	for i := 0; i < len(nameParts)-1; i++ {
		name := strings.Join(nameParts[i:], ".")
		// Test for an SOA (which tells us we have authority)
		found, err := cfg.db.HasDNS(name, "SOA")
		if err == nil && found {
			return true
		}
		// Test for a DNAME which has special handling for aliasing of subdomains within
		found, err = cfg.db.HasDNS(name, "DNAME")
		if err == nil && found {
			// FIXME!  THIS NEEDS TO HANDLE DNAME ALIASING CORRECTLY INSTEAD OF IGNORING IT...
			log.Printf("DNAME EXISTS!  WE NEED TO HANDLE THIS CORRECTLY... FIXME\n")
			return true
		}
	}
	return false
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
