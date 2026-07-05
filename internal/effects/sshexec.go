package effects

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHRunner runs one-shot commands on a host over SSH with public-key auth. The
// CMC accepts key auth only on its `service` account, so User is "service" and
// the key is nut-dog's dedicated RSA key (from the k8s secret).
type SSHRunner struct {
	User      string
	signer    ssh.Signer
	hostKeyCb ssh.HostKeyCallback
	timeout   time.Duration
}

// NewSSHRunner builds a runner from a PEM/OpenSSH private key. If hostKey is a
// non-empty authorized_keys line, the CMC's host key is verified against it;
// otherwise verification is disabled (trusted mgmt VLAN).
func NewSSHRunner(user string, privateKey []byte, hostKey string, timeout time.Duration) (*SSHRunner, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	hostKeyCb := ssh.InsecureIgnoreHostKey()
	if hostKey != "" {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hostKey))
		if err != nil {
			return nil, fmt.Errorf("parse cmc host key: %w", err)
		}
		hostKeyCb = ssh.FixedHostKey(pub)
	}
	return &SSHRunner{User: user, signer: signer, hostKeyCb: hostKeyCb, timeout: timeout}, nil
}

// Run dials the host, runs cmd in a single session, and returns combined
// stdout+stderr. A non-zero remote exit is returned as an error.
func (s *SSHRunner) Run(host, cmd string) (string, error) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "22")
	}
	cfg := &ssh.ClientConfig{
		User:            s.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(s.signer)},
		HostKeyCallback: s.hostKeyCb,
		Timeout:         s.timeout,
	}
	client, err := ssh.Dial("tcp", host, cfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}
