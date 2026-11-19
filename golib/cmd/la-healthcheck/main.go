// Command la-healthcheck is the container health probe for the distroless
// images, which have no shell, curl or wget:
//
//	la-healthcheck http://127.0.0.1:8080/healthz
//
// It exits 0 when the URL answers 2xx within the timeout and 1 otherwise.
// Only plain http:// to a host:port is supported; it speaks just enough
// HTTP/1.0 to read a status line, so the binary stays small.
package main

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	timeout := 3 * time.Second
	args := os.Args[1:]
	if len(args) == 3 && args[0] == "-timeout" {
		d, err := time.ParseDuration(args[1])
		if err != nil {
			fail(2, "bad -timeout: %v", err)
		}
		timeout, args = d, args[2:]
	}
	if len(args) != 1 {
		fail(2, "usage: la-healthcheck [-timeout 3s] http://host:port/path")
	}
	if err := probe(args[0], timeout); err != nil {
		fail(1, "%v", err)
	}
}

func fail(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "la-healthcheck: "+format+"\n", a...)
	os.Exit(code)
}

func probe(raw string, timeout time.Duration) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("only http://host:port/path is supported, got %q", raw)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	path := u.RequestURI()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: la-healthcheck\r\nConnection: close\r\n\r\n", path, u.Host); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("no status line: %w", err)
	}
	// "HTTP/1.1 200 OK"
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return fmt.Errorf("unexpected reply %q", strings.TrimSpace(line))
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("unexpected status %q", parts[1])
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s answered %d", raw, status)
	}
	return nil
}
