package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe ADDRESS SERVER-NAME")
		os.Exit(2)
	}
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", os.Args[1], &tls.Config{ServerName: os.Args[2], MinVersion: tls.VersionTLS12})
	if err == nil {
		_ = connection.Close()
		fmt.Println("trusted")
		return
	}
	var hostname x509.HostnameError
	var authority x509.UnknownAuthorityError
	kind := "other"
	if errors.As(err, &hostname) {
		kind = "hostname"
	}
	if errors.As(err, &authority) {
		kind = "authority"
	}
	fmt.Fprintln(os.Stderr, kind+": "+err.Error())
	os.Exit(1)
}
