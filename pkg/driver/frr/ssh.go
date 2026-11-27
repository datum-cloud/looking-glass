package frr

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"go.datum.net/looking-glass/pkg/driver"
)

// sshHandshakeTimeout bounds the SSH handshake when the caller's context has
// a later deadline.
const sshHandshakeTimeout = 10 * time.Second

// SSHRunner runs each command in a new SSH connection to the router, so a
// broken session never affects another diagnostic.
type SSHRunner struct {
	addr   string
	config *ssh.ClientConfig
}

// NewSSHRunner validates the SSH settings and reads the key and known_hosts
// files. It performs no network I/O.
func NewSSHRunner(address string, settings map[string]string) (*SSHRunner, error) {
	user, keyFile, hostsFile := settings[SettingUser], settings[SettingPrivateKeyFile], settings[SettingKnownHostsFile]
	if user == "" || keyFile == "" || hostsFile == "" {
		return nil, fmt.Errorf("settings %s, %s and %s are required", SettingUser, SettingPrivateKeyFile, SettingKnownHostsFile)
	}
	if address == "" {
		return nil, errors.New("an address is required")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "22")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", SettingPrivateKeyFile, err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", SettingPrivateKeyFile, err)
	}
	hostKey, err := knownhosts.New(hostsFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", SettingKnownHostsFile, err)
	}
	algorithms, err := hostKeyAlgorithms(hostKey, address)
	if err != nil {
		return nil, err
	}
	return &SSHRunner{addr: address, config: &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: algorithms,
		Timeout:           sshHandshakeTimeout,
	}}, nil
}

// hostKeyAlgorithms returns the host key algorithms of the keys known_hosts
// holds for address. The client offers only those, so a router with several
// host keys presents one the file can verify. It fails when the file holds
// no key for address.
func hostKeyAlgorithms(callback ssh.HostKeyCallback, address string) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	remote := &net.TCPAddr{IP: net.ParseIP(host)}
	remote.Port, _ = strconv.Atoi(port)
	if remote.IP == nil {
		remote.IP = net.IPv4zero
	}
	// A fresh key is in no known_hosts file, so the callback reports every
	// key it knows for the host.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	var known *knownhosts.KeyError
	if err := callback(address, remote, probe); !errors.As(err, &known) || len(known.Want) == 0 {
		return nil, fmt.Errorf("%s holds no host key for %s", SettingKnownHostsFile, address)
	}
	var algorithms []string
	for _, k := range known.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			algorithms = append(algorithms, t)
		}
	}
	return algorithms, nil
}

// Run connects, runs command and returns at most limit bytes of its standard
// output. It closes the connection as soon as ctx is done.
func (r *SSHRunner) Run(ctx context.Context, command string, limit int) ([]byte, error) {
	conn, err := (&net.Dialer{Timeout: sshHandshakeTimeout}).DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return nil, &driver.Error{Reason: driver.ReasonRouterUnreachable, Message: "could not connect to the router", Err: err}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, r.addr, r.config)
	if err != nil {
		return nil, &driver.Error{Reason: driver.ReasonRouterUnavailable, Message: "SSH handshake with the router failed", Err: err}
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return nil, &driver.Error{Reason: driver.ReasonRouterUnavailable, Message: "the router refused an SSH session", Err: err}
	}
	defer func() { _ = session.Close() }()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, &driver.Error{Reason: driver.ReasonRouterUnavailable, Message: "the router refused the command", Err: err}
	}
	var out bytes.Buffer
	n, err := io.Copy(&out, io.LimitReader(stdout, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if n > int64(limit) {
		return nil, driver.Errorf(driver.ReasonResponseTooLarge, "the router's output exceeded %d bytes", limit)
	}
	if err := session.Wait(); err != nil {
		if exit, ok := errors.AsType[*ssh.ExitError](err); ok {
			return out.Bytes(), &ExitError{Status: exit.ExitStatus()}
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// Close is a no-op: the runner holds no connection between commands.
func (r *SSHRunner) Close() error { return nil }
