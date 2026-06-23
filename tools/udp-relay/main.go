package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type session struct {
	server *net.UDPConn
	client *net.UDPAddr
	last   time.Time
}

func main() {
	targetHost := env("TARGET_HOST", "192.168.58.2")
	first, last, err := parseRange(env("PORT_RANGE", "7000-8000"))
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("udp relay target=%s ports=%d-%d", targetHost, first, last)
	var wg sync.WaitGroup
	for port := first; port <= last; port++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			if err := servePort(targetHost, port); err != nil {
				log.Printf("port %d stopped: %v", port, err)
			}
		}(port)
	}
	wg.Wait()
}

func servePort(targetHost string, port int) error {
	listenAddr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	listener, err := net.ListenUDP("udp4", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	targetAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", targetHost, port))
	if err != nil {
		return err
	}

	sessions := map[string]*session{}
	var mu sync.Mutex
	go cleanupSessions(&mu, sessions)

	buf := make([]byte, 65535)
	for {
		n, clientAddr, err := listener.ReadFromUDP(buf)
		if err != nil {
			return err
		}
		key := clientAddr.String()

		mu.Lock()
		s := sessions[key]
		if s == nil {
			serverConn, err := net.DialUDP("udp4", nil, targetAddr)
			if err != nil {
				mu.Unlock()
				log.Printf("port %d dial target failed for %s: %v", port, key, err)
				continue
			}
			s = &session{server: serverConn, client: clientAddr, last: time.Now()}
			sessions[key] = s
			go relayResponses(listener, serverConn, clientAddr)
		}
		s.last = time.Now()
		mu.Unlock()

		if _, err := s.server.Write(buf[:n]); err != nil {
			log.Printf("port %d write target failed for %s: %v", port, key, err)
		}
	}
}

func relayResponses(listener *net.UDPConn, server *net.UDPConn, client *net.UDPAddr) {
	buf := make([]byte, 65535)
	for {
		n, err := server.Read(buf)
		if err != nil {
			return
		}
		if _, err := listener.WriteToUDP(buf[:n], client); err != nil {
			log.Printf("write client %s failed: %v", client, err)
			return
		}
	}
}

func cleanupSessions(mu *sync.Mutex, sessions map[string]*session) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-2 * time.Minute)
		mu.Lock()
		for key, s := range sessions {
			if s.last.Before(cutoff) {
				_ = s.server.Close()
				delete(sessions, key)
			}
		}
		mu.Unlock()
	}
}

func parseRange(raw string) (int, int, error) {
	parts := strings.Split(raw, "-")
	if len(parts) == 1 {
		port, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		return port, port, err
	}
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid PORT_RANGE %q", raw)
	}
	first, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	last, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	if first <= 0 || last < first || last > 65535 {
		return 0, 0, fmt.Errorf("invalid PORT_RANGE %q", raw)
	}
	return first, last, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
