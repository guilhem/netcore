package main

import (
	"fmt"
	"os"
)

func main() {
	etc := etcdSetup()
	dhcpExit := dhcpSetup(etc)

	select {
	case <-dhcpExit:
		fmt.Println("DHCP Exited")
		os.Exit(1)
	}
}