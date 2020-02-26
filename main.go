package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Endpoint provides the details required to forward remote services to the
// localhost.
type Endpoint struct {
	Name       string `json:"name"`
	LocalAddr  string `json:"local"`
	RemoteAddr string `json:"remote"`
}

// Host is a host.
type Host struct {
	Address   string     `json:"address"`
	Endpoints []Endpoint `json:"endpoints"`
	Name      string     `json:"name"`
}

// Config provides the full list of hosts and their associated endpoints.
type Config struct {
	Environment string `json:"environment"`
	Hosts       []Host `json:"hosts"`
}

func main() {
	var filename string
	var username string

	flag.StringVar(&filename, "f", "", "file containing environment hosts and endpoints. (required)")
	flag.StringVar(&username, "u", "", "ssh user name to use when connecting to the hosts. (required)")
	flag.Parse()

	if filename == "" || username == "" {
		flag.Usage()
		return
	}

	var envConfig Config

	r, err := os.Open(filename)
	if err != nil {
		log.Fatalf("Failed to open config: %v", err)
	}

	dec := json.NewDecoder(r)
	err = dec.Decode(&envConfig)
	if err != nil {
		log.Fatalf("Failed to unmarshal config: %v", err)
	}

	// ssh-agent(1) provides a UNIX socket at $SSH_AUTH_SOCK.
	socket := os.Getenv("SSH_AUTH_SOCK")
	agentConn, err := net.Dial("unix", socket)
	if err != nil {
		log.Fatalf("Failed to open SSH_AUTH_SOCK: %v", err)
	}

	agentClient := agent.NewClient(agentConn)
	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			// Use a callback rather than PublicKeys so we only consult the
			// agent once the remote server wants it.
			ssh.PublicKeysCallback(agentClient.Signers),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	log.Printf("Initiating tunnels for %s\n", envConfig.Environment)

	for _, host := range envConfig.Hosts {
		log.Printf("Connecting to %v <%v>\n", host.Name, host.Address)
		client, err := ssh.Dial("tcp", host.Address, config)
		if err != nil {
			log.Fatal(err)
		}
		defer client.Close()

		for _, endpoint := range host.Endpoints {
			go forwardEndpoint(client, endpoint)
		}
	}

	log.Fatal(http.ListenAndServe(":0", nil))
}

// forwardEndpoint adds port forwarding from a remote service to a locally bound address.
func forwardEndpoint(client *ssh.Client, endpoint Endpoint) {
	log.Printf("Forwarding %v from <%v> to <%v>", endpoint.Name, endpoint.RemoteAddr, endpoint.LocalAddr)

	local, err := net.Listen("tcp", endpoint.LocalAddr)
	if err != nil {
		log.Printf("forwarding port bind error: %v\n", err)
		return
	}

	// local connection Accept loop.
	for {
		forward, err := local.Accept()
		if err != nil {
			log.Printf("local accept error: %v", err)
			return
		}

		remote, err := client.Dial("tcp", endpoint.RemoteAddr)
		if err != nil {
			log.Printf("remote dial error: %v", err)
			continue
		}

		go handleClient(forward, remote)
	}
}

func handleClient(forward net.Conn, remote net.Conn) {
	close := func() {
		// TODO: need to improve the signalling that a connection is closed for
		// the go-routines that follow.
		forward.Close()
		remote.Close()
	}

	// Start remote -> local data transfer
	go func(f net.Conn, r net.Conn) {
		defer close()
		_, err := io.Copy(f, r)
		if err != nil && err != io.EOF {
			log.Printf("copy <remote->local> error: %v\n", err)
		}
	}(forward, remote)

	// Start local -> remote data transfer
	go func(f net.Conn, r net.Conn) {
		defer close()
		_, err := io.Copy(r, f)
		if err != nil && err != io.EOF {
			log.Printf("copy <local->remote> error: %v\n", err)
		}
	}(forward, remote)
}
