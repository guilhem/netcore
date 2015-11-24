package main

import (
	"os/exec"
	"strings"

	"code.google.com/p/go-uuid/uuid"
)

func getHostname() (string, error) {
	fqdn, err := exec.Command("hostname", "-f").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(fqdn)), nil
}

func reverseSlice(in []string) []string {
	out := make([]string, len(in))
	for i := range in {
		out[len(in)-i-1] = in[i]
	}
	return out
}

func getUUID() string {
	return uuid.New()
}
