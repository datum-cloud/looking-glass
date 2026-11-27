package frr_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"go.datum.net/looking-glass/pkg/driver"
	"go.datum.net/looking-glass/pkg/driver/frr"
)

// sshServer is a minimal SSH server that runs no command: it answers each
// exec request from commands, or blocks until the client goes away for
// "hang".
type sshServer struct {
	addr string
	// hostKey is the ed25519 key; the server also presents an ECDSA key that
	// no test writes to known_hosts.
	hostKey  ssh.PublicKey
	commands map[string]struct {
		out    string
		status uint32
	}
}

func startSSHServer(t *testing.T, clientKey ssh.PublicKey) *sshServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(clientKey.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	cfg.AddHostKey(hostSigner)
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddHostKey(ecdsaSigner)
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	s := &sshServer{addr: lis.Addr().String(), hostKey: hostSigner.PublicKey(), commands: map[string]struct {
		out    string
		status uint32
	}{
		"echo ok": {"ok\n", 0},
		"fail":    {"partial\n", 3},
		"big":     {strings.Repeat("x", 100), 0},
	}}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, cfg)
		}
	}()
	return s
}

func (s *sshServer) handle(conn net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = conn.Close() }()
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range requests {
				if req.Type != "exec" || len(req.Payload) < 4 {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				command := string(req.Payload[4:])
				if command == "hang" {
					// Hold the channel open until the client closes it.
					for range requests {
					}
					return
				}
				c := s.commands[command]
				_, _ = ch.Write([]byte(c.out))
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, c.status)
				_, _ = ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}

// clientFiles writes a client key and returns the driver settings that use
// it, with a known_hosts path writeKnownHosts fills.
func clientFiles(t *testing.T) (map[string]string, ssh.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	clientKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		frr.SettingUser: "lg", frr.SettingPrivateKeyFile: keyFile, frr.SettingKnownHostsFile: filepath.Join(dir, "known_hosts"),
	}, clientKey
}

func writeKnownHosts(t *testing.T, settings map[string]string, addr string, hostKey ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(addr)}, hostKey)
	if err := os.WriteFile(settings[frr.SettingKnownHostsFile], []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func otherHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestSSHRunner(t *testing.T) {
	settings, clientKey := clientFiles(t)
	srv := startSSHServer(t, clientKey)
	writeKnownHosts(t, settings, srv.addr, srv.hostKey)
	runner, err := frr.NewSSHRunner(srv.addr, settings)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runner.Run(t.Context(), "echo ok", 1024)
	if err != nil || string(out) != "ok\n" {
		t.Errorf("Run(echo ok) = %q, %v", out, err)
	}

	out, err = runner.Run(t.Context(), "fail", 1024)
	var exit *frr.ExitError
	if !errors.As(err, &exit) || exit.Status != 3 || string(out) != "partial\n" {
		t.Errorf("Run(fail) = %q, %v; want the output and exit status 3", out, err)
	}

	_, err = runner.Run(t.Context(), "big", 10)
	if driver.ReasonOf(err) != driver.ReasonResponseTooLarge {
		t.Errorf("Run(big) with a 10-byte limit: %v, want ResponseTooLarge", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runner.Run(ctx, "hang", 1024)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("Run(hang) returned %v after %s, want an error once the context ends", err, time.Since(start))
	}
}

func TestSSHRunnerRejectsUnknownHostKey(t *testing.T) {
	settings, clientKey := clientFiles(t)
	srv := startSSHServer(t, clientKey)
	// known_hosts holds a different key for the server's address.
	writeKnownHosts(t, settings, srv.addr, otherHostKey(t))
	runner, err := frr.NewSSHRunner(srv.addr, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), "echo ok", 1024); driver.ReasonOf(err) != driver.ReasonRouterUnavailable {
		t.Errorf("Run with a mismatched host key: %v, want RouterUnavailable", err)
	}
}

func TestSSHRunnerUnreachable(t *testing.T) {
	lis, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	settings, _ := clientFiles(t)
	writeKnownHosts(t, settings, addr, otherHostKey(t))
	runner, err := frr.NewSSHRunner(addr, settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), "echo ok", 1024); driver.ReasonOf(err) != driver.ReasonRouterUnreachable {
		t.Errorf("Run against a closed port: %v, want RouterUnreachable", err)
	}
}

func TestSSHRunnerNeedsKnownHost(t *testing.T) {
	settings, _ := clientFiles(t)
	writeKnownHosts(t, settings, "192.0.2.99:22", otherHostKey(t))
	if _, err := frr.NewSSHRunner("192.0.2.10", settings); err == nil || !strings.Contains(err.Error(), "no host key for 192.0.2.10:22") {
		t.Errorf("NewSSHRunner for a host missing from known_hosts = %v, want an error naming it", err)
	}
}

func TestSSHRunnerMatchesAddressAsConfigured(t *testing.T) {
	settings, _ := clientFiles(t)
	// An entry for the IP address does not cover a hostname address.
	writeKnownHosts(t, settings, "192.0.2.10", otherHostKey(t))
	if _, err := frr.NewSSHRunner("router.example.com", settings); err == nil {
		t.Error("NewSSHRunner accepted a hostname address whose known_hosts entry is keyed by IP only")
	}
	writeKnownHosts(t, settings, "router.example.com", otherHostKey(t))
	if _, err := frr.NewSSHRunner("router.example.com", settings); err != nil {
		t.Errorf("NewSSHRunner with a hostname entry: %v", err)
	}
}
