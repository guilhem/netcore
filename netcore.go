package main

import (
	"flag"
	"log"
	"os"
	"strings"
)

var etcdServers = flag.String("etcd", "http://127.0.0.1:2379", "Comma-separated list of etcd servers.")

func init() {
	flag.Parse()
}

func main() {
	if len(*etcdServers) == 0 {
		if len(os.Getenv("ETCD_PORT")) > 0 {
			*etcdServers = strings.Replace(os.Getenv("ETCD_PORT"), "tcp://", "http://", 1)
		} else {
			*etcdServers = "etcd" // just some default hostname that Docker or otherwise might use
		}
	}
	db := NewEtcdDB(*etcdServers)

	log.Println("PRECONFIG")
	cfg, err := db.GetConfig()
	log.Println("POSTCONFIG")

	if err != nil {
		log.Printf("Configuration failed: %s\n", err)
		os.Exit(1)
	}

	var dhcpExit chan error
	if cfg.DHCPIP() == nil {
		log.Println("DHCP service is disabled; this machine does not have a DHCP IP assigned.")
	} else if cfg.DHCPSubnet() == nil {
		log.Println("DHCP service is disabled; this machine's zone does not have a DHCP subnet assigned.")
	} else if cfg.DHCPNIC() == "" {
		log.Println("DHCP service is disabled; this machine does not have a DHCP NIC assigned.")
	} else {
		dhcpExit = dhcpSetup(cfg)
	}

	dnsExit := dnsSetup(cfg)

	log.Println("NETCORE Started.")

	select {
	case err := <-dhcpExit:
		log.Printf("DHCP Exited: %s\n", err)
		os.Exit(1)
	case err := <-dnsExit:
		log.Printf("DNS Exited: %s\n", err)
		os.Exit(1)
	}
}
