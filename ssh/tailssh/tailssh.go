// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux || (darwin && !ios) || freebsd || openbsd

// Package tailssh is an SSH server integrated into Tailscale.
package tailssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gossh "github.com/tailscale/golang-x-crypto/ssh"
	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/tsaddr"
	"tailscale.com/net/tsdial"
	"tailscale.com/tailcfg"
	"tailscale.com/tempfork/gliderlabs/ssh"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/mak"
	"tailscale.com/util/multierr"
	"tailscale.com/version/distro"
)

var (
	sshVerboseLogging = envknob.RegisterBool("TS_DEBUG_SSH_VLOG")
)

const (
	// forcePasswordSuffix is the suffix at the end of a username that forces
	// Tailscale SSH into password authentication mode to work around buggy SSH
	// clients that get confused by successful replies to auth type "none".
	forcePasswordSuffix = "+password"
)

// ipnLocalBackend is the subset of ipnlocal.LocalBackend that we use.
// It is used for testing.
type ipnLocalBackend interface {
	GetSSH_HostKeys() ([]gossh.Signer, error)
	ShouldRunSSH() bool
	NetMap() *netmap.NetworkMap
	WhoIs(ipp netip.AddrPort) (n *tailcfg.Node, u tailcfg.UserProfile, ok bool)
	DoNoiseRequest(req *http.Request) (*http.Response, error)
	Dialer() *tsdial.Dialer
	TailscaleVarRoot() string
	NodeKey() key.NodePublic
}

type server struct {
	lb             ipnLocalBackend
	logf           logger.Logf
	tailscaledPath string

	pubKeyHTTPClient *http.Client     // or nil for http.DefaultClient
	timeNow          func() time.Time // or nil for time.Now

	sessionWaitGroup sync.WaitGroup

	// mu protects the following
	mu                   sync.Mutex
	activeConns          map[*conn]bool              // set; value is always true
	fetchPublicKeysCache map[string]pubKeyCacheEntry // by https URL
	shutdownCalled       bool
}

func (srv *server) now() time.Time {
	if srv != nil && srv.timeNow != nil {
		return srv.timeNow()
	}
	return time.Now()
}

func init() {
	ipnlocal.RegisterNewSSHServer(func(logf logger.Logf, lb *ipnlocal.LocalBackend) (ipnlocal.SSHServer, error) {
		tsd, err := os.Executable()
		if err != nil {
			return nil, err
		}
		srv := &server{
			lb:             lb,
			logf:           logf,
			tailscaledPath: tsd,
		}

		return srv, nil
	})
}

// attachSessionToConnIfNotShutdown ensures that srv is not shutdown before
// attaching the session to the conn. This ensures that once Shutdown is called,
// new sessions are not allowed and existing ones are cleaned up.
// It reports whether ss was attached to the conn.
func (srv *server) attachSessionToConnIfNotShutdown(ss *sshSession) bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.shutdownCalled {
		// Do not start any new sessions.
		return false
	}
	ss.conn.attachSession(ss)
	return true
}

func (srv *server) trackActiveConn(c *conn, add bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if add {
		mak.Set(&srv.activeConns, c, true)
		return
	}
	delete(srv.activeConns, c)
}

// HandleSSHConn handles a Tailscale SSH connection from c.
// This is the entry point for all SSH connections.
// When this returns, the connection is closed.
func (srv *server) HandleSSHConn(nc net.Conn) error {
	metricIncomingConnections.Add(1)
	c, err := srv.newConn()
	if err != nil {
		return err
	}
	srv.trackActiveConn(c, true)        // add
	defer srv.trackActiveConn(c, false) // remove
	c.HandleConn(nc)

	// Return nil to signal to netstack's interception that it doesn't need to
	// log. If ss.HandleConn had problems, it can log itself (ideally on an
	// sshSession.logf).
	return nil
}

// Shutdown terminates all active sessions.
func (srv *server) Shutdown() {
	srv.mu.Lock()
	srv.shutdownCalled = true
	for c := range srv.activeConns {
		c.Close()
	}
	srv.mu.Unlock()
	srv.sessionWaitGroup.Wait()
}

// OnPolicyChange terminates any active sessions that no longer match
// the SSH access policy.
func (srv *server) OnPolicyChange() {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for c := range srv.activeConns {
		if c.info == nil {
			// c.info is nil when the connection hasn't been authenticated yet.
			// In that case, the connection will be terminated when it is.
			continue
		}
		go c.checkStillValid()
	}
}

// conn represents a single SSH connection and its associated
// ssh.Server.
//
// During the lifecycle of a connection, the following are called in order:
// Setup and discover server info
//   - ServerConfigCallback
//
// Do the user auth
//   - NoClientAuthHandler
//   - PublicKeyHandler (only if NoClientAuthHandler returns errPubKeyRequired)
//
// Once auth is done, the conn can be multiplexed with multiple sessions and
// channels concurrently. At which point any of the following can be called
// in any order.
//   - c.handleSessionPostSSHAuth
//   - c.mayForwardLocalPortTo followed by ssh.DirectTCPIPHandler
type conn struct {
	*ssh.Server
	srv *server

	insecureSkipTailscaleAuth bool // used by tests.

	// idH is the RFC4253 sec8 hash H. It is used to identify the connection,
	// and is shared among all sessions. It should not be shared outside
	// process. It is confusingly referred to as SessionID by the gliderlabs/ssh
	// library.
	idH    string
	connID string // ID that's shared with control

	// anyPasswordIsOkay is whether the client is authorized but has requested
	// password-based auth to work around their buggy SSH client. When set, we
	// accept any password in the PasswordHandler.
	anyPasswordIsOkay bool // set by NoClientAuthCallback

	action0        *tailcfg.SSHAction // set by doPolicyAuth; first matching action
	currentAction  *tailcfg.SSHAction // set by doPolicyAuth, updated by resolveNextAction
	finalAction    *tailcfg.SSHAction // set by doPolicyAuth or resolveNextAction
	finalActionErr error              // set by doPolicyAuth or resolveNextAction

	info         *sshConnInfo    // set by setInfo
	localUser    *user.User      // set by doPolicyAuth
	userGroupIDs []string        // set by doPolicyAuth
	pubKey       gossh.PublicKey // set by doPolicyAuth

	// mu protects the following fields.
	//
	// srv.mu should be acquired prior to mu.
	// It is safe to just acquire mu, but unsafe to
	// acquire mu and then srv.mu.
	mu       sync.Mutex // protects the following
	sessions []*sshSession
}

func (c *conn) logf(format string, args ...any) {
	format = fmt.Sprintf("%v: %v", c.connID, format)
	c.srv.logf(format, args...)
}

func (c *conn) vlogf(format string, args ...any) {
	if sshVerboseLogging() {
		c.logf(format, args...)
	}
}

// isAuthorized walks through the action chain and returns nil if the connection
// is authorized. If the connection is not authorized, it returns
// gossh.ErrDenied. If the action chain resolution fails, it returns the
// resolution error.
func (c *conn) isAuthorized(ctx ssh.Context) error {
	action := c.currentAction
	for {
		if action.Accept {
			if c.pubKey != nil {
				metricPublicKeyAccepts.Add(1)
			}
			return nil
		}
		if action.Reject || action.HoldAndDelegate == "" {
			return gossh.ErrDenied
		}
		var err error
		action, err = c.resolveNextAction(ctx)
		if err != nil {
			return err
		}
		if action.Message != "" {
			if err := ctx.SendAuthBanner(action.Message); err != nil {
				return err
			}
		}
	}
}

// errPubKeyRequired is returned by NoClientAuthCallback to make the client
// resort to public-key auth; not user visible.
var errPubKeyRequired = errors.New("ssh publickey required")

// NoClientAuthCallback implements gossh.NoClientAuthCallback and is called by
// the ssh.Server when the client first connects with the "none"
// authentication method.
//
// It is responsible for continuing policy evaluation from BannerCallback (or
// starting it afresh). It returns an error if the policy evaluation fails, or
// if the decision is "reject"
//
// It either returns nil (accept) or errPubKeyRequired or gossh.ErrDenied
// (reject). The errors may be wrapped.
func (c *conn) NoClientAuthCallback(ctx ssh.Context) error {
	if c.insecureSkipTailscaleAuth {
		return nil
	}
	if err := c.doPolicyAuth(ctx, nil /* no pub key */); err != nil {
		return err
	}
	if err := c.isAuthorized(ctx); err != nil {
		return err
	}

	// Let users specify a username ending in +password to force password auth.
	// This exists for buggy SSH clients that get confused by success from
	// "none" auth.
	if strings.HasSuffix(ctx.User(), forcePasswordSuffix) {
		c.anyPasswordIsOkay = true
		return errors.New("any password please") // not shown to users
	}
	return nil
}

func (c *conn) nextAuthMethodCallback(cm gossh.ConnMetadata, prevErrors []error) (nextMethod []string) {
	switch {
	case c.anyPasswordIsOkay:
		nextMethod = append(nextMethod, "password")
	case len(prevErrors) > 0 && prevErrors[len(prevErrors)-1] == errPubKeyRequired:
		nextMethod = append(nextMethod, "publickey")
	}

	// The fake "tailscale" method is always appended to next so OpenSSH renders
	// that in parens as the final failure. (It also shows up in "ssh -v", etc)
	nextMethod = append(nextMethod, "tailscale")
	return
}

// fakePasswordHandler is our implementation of the PasswordHandler hook that
// checks whether the user's password is correct. But we don't actually use
// passwords. This exists only for when the user's username ends in "+password"
// to signal that their SSH client is buggy and gets confused by auth type
// "none" succeeding and they want our SSH server to require a dummy password
// prompt instead. We then accept any password since we've already authenticated
// & authorized them.
func (c *conn) fakePasswordHandler(ctx ssh.Context, password string) bool {
	return c.anyPasswordIsOkay
}

// PublicKeyHandler implements ssh.PublicKeyHandler is called by the
// ssh.Server when the client presents a public key.
func (c *conn) PublicKeyHandler(ctx ssh.Context, pubKey ssh.PublicKey) error {
	if err := c.doPolicyAuth(ctx, pubKey); err != nil {
		// TODO(maisem/bradfitz): surface the error here.
		c.logf("rejecting SSH public key %s: %v", bytes.TrimSpace(gossh.MarshalAuthorizedKey(pubKey)), err)
		return err
	}
	if err := c.isAuthorized(ctx); err != nil {
		return err
	}
	c.logf("accepting SSH public key %s", bytes.TrimSpace(gossh.MarshalAuthorizedKey(pubKey)))
	return nil
}

// doPolicyAuth verifies that conn can proceed with the specified (optional)
// pubKey. It returns nil if the matching policy action is Accept or
// HoldAndDelegate. If pubKey is nil, there was no policy match but there is a
// policy that might match a public key it returns errPubKeyRequired. Otherwise,
// it returns gossh.ErrDenied.
func (c *conn) doPolicyAuth(ctx ssh.Context, pubKey ssh.PublicKey) error {
	if err := c.setInfo(ctx); err != nil {
		c.logf("failed to get conninfo: %v", err)
		return gossh.ErrDenied
	}
	a, localUser, err := c.evaluatePolicy(pubKey)
	if err != nil {
		if pubKey == nil && c.havePubKeyPolicy() {
			return errPubKeyRequired
		}
		return fmt.Errorf("%w: %v", gossh.ErrDenied, err)
	}
	c.action0 = a
	c.currentAction = a
	c.pubKey = pubKey
	if a.Message != "" {
		if err := ctx.SendAuthBanner(a.Message); err != nil {
			return fmt.Errorf("SendBanner: %w", err)
		}
	}
	if a.Accept || a.HoldAndDelegate != "" {
		if a.Accept {
			c.finalAction = a
		}
		if runtime.GOOS == "linux" && distro.Get() == distro.Gokrazy {
			// Gokrazy is a single-user appliance with ~no userspace.
			// There aren't users to look up (no /etc/passwd, etc)
			// so rather than fail below, just hardcode root.
			// TODO(bradfitz): fix os/user upstream instead?
			c.userGroupIDs = []string{"0"}
			c.localUser = &user.User{Uid: "0", Gid: "0", Username: "root"}
			return nil
		}
		lu, err := user.Lookup(localUser)
		if err != nil {
			c.logf("failed to look up %v: %v", localUser, err)
			ctx.SendAuthBanner(fmt.Sprintf("failed to look up %v\r\n", localUser))
			return err
		}
		gids, err := lu.GroupIds()
		if err != nil {
			c.logf("failed to look up local user's group IDs: %v", err)
			return err
		}
		c.userGroupIDs = gids
		c.localUser = lu
		return nil
	}
	if a.Reject {
		c.finalAction = a
		return gossh.ErrDenied
	}
	// Shouldn't get here, but:
	return gossh.ErrDenied
}

// ServerConfig implements ssh.ServerConfigCallback.
func (c *conn) ServerConfig(ctx ssh.Context) *gossh.ServerConfig {
	return &gossh.ServerConfig{
		NoClientAuth:           true, // required for the NoClientAuthCallback to run
		NextAuthMethodCallback: c.nextAuthMethodCallback,
	}
}

func (srv *server) newConn() (*conn, error) {
	srv.mu.Lock()
	if srv.shutdownCalled {
		srv.mu.Unlock()
		// Stop accepting new connections.
		// Connections in the auth phase are handled in handleConnPostSSHAuth.
		// Existing sessions are terminated by Shutdown.
		return nil, gossh.ErrDenied
	}
	srv.mu.Unlock()
	c := &conn{srv: srv}
	now := srv.now()
	c.connID = fmt.Sprintf("ssh-conn-%s-%02x", now.UTC().Format("20060102T150405"), randBytes(5))
	c.Server = &ssh.Server{
		Version:              "Tailscale",
		ServerConfigCallback: c.ServerConfig,

		NoClientAuthHandler: c.NoClientAuthCallback,
		PublicKeyHandler:    c.PublicKeyHandler,
		PasswordHandler:     c.fakePasswordHandler,

		Handler:                     c.handleSessionPostSSHAuth,
		LocalPortForwardingCallback: c.mayForwardLocalPortTo,
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": c.handleSessionPostSSHAuth,
		},
		// Note: the direct-tcpip channel handler and LocalPortForwardingCallback
		// only adds support for forwarding ports from the local machine.
		// TODO(maisem/bradfitz): add remote port forwarding support.
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		RequestHandlers: map[string]ssh.RequestHandler{},
	}
	ss := c.Server
	for k, v := range ssh.DefaultRequestHandlers {
		ss.RequestHandlers[k] = v
	}
	for k, v := range ssh.DefaultChannelHandlers {
		ss.ChannelHandlers[k] = v
	}
	for k, v := range ssh.DefaultSubsystemHandlers {
		ss.SubsystemHandlers[k] = v
	}
	keys, err := srv.lb.GetSSH_HostKeys()
	if err != nil {
		return nil, err
	}
	for _, signer := range keys {
		ss.AddHostKey(signer)
	}
	return c, nil
}

// mayForwardLocalPortTo reports whether the ctx should be allowed to port forward
// to the specified host and port.
// TODO(bradfitz/maisem): should we have more checks on host/port?
func (c *conn) mayForwardLocalPortTo(ctx ssh.Context, destinationHost string, destinationPort uint32) bool {
	if c.finalAction != nil && c.finalAction.AllowLocalPortForwarding {
		metricLocalPortForward.Add(1)
		return true
	}
	return false
}

// havePubKeyPolicy reports whether any policy rule may provide access by means
// of a ssh.PublicKey.
func (c *conn) havePubKeyPolicy() bool {
	if c.info == nil {
		panic("havePubKeyPolicy called before setInfo")
	}
	// Is there any rule that looks like it'd require a public key for this
	// sshUser?
	pol, ok := c.sshPolicy()
	if !ok {
		return false
	}
	for _, r := range pol.Rules {
		if c.ruleExpired(r) {
			continue
		}
		if mapLocalUser(r.SSHUsers, c.info.sshUser) == "" {
			continue
		}
		for _, p := range r.Principals {
			if len(p.PubKeys) > 0 && c.principalMatchesTailscaleIdentity(p) {
				return true
			}
		}
	}
	return false
}

// sshPolicy returns the SSHPolicy for current node.
// If there is no SSHPolicy in the netmap, it returns a debugPolicy
// if one is defined.
func (c *conn) sshPolicy() (_ *tailcfg.SSHPolicy, ok bool) {
	lb := c.srv.lb
	if !lb.ShouldRunSSH() {
		return nil, false
	}
	nm := lb.NetMap()
	if nm == nil {
		return nil, false
	}
	if pol := nm.SSHPolicy; pol != nil && !envknob.SSHIgnoreTailnetPolicy() {
		return pol, true
	}
	debugPolicyFile := envknob.SSHPolicyFile()
	if debugPolicyFile != "" {
		c.logf("reading debug SSH policy file: %v", debugPolicyFile)
		f, err := os.ReadFile(debugPolicyFile)
		if err != nil {
			c.logf("error reading debug SSH policy file: %v", err)
			return nil, false
		}
		p := new(tailcfg.SSHPolicy)
		if err := json.Unmarshal(f, p); err != nil {
			c.logf("invalid JSON in %v: %v", debugPolicyFile, err)
			return nil, false
		}
		return p, true
	}
	return nil, false
}

func toIPPort(a net.Addr) (ipp netip.AddrPort) {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return
	}
	tanetaddr, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return
	}
	return netip.AddrPortFrom(tanetaddr.Unmap(), uint16(ta.Port))
}

// connInfo returns a populated sshConnInfo from the provided arguments,
// validating only that they represent a known Tailscale identity.
func (c *conn) setInfo(ctx ssh.Context) error {
	if c.info != nil {
		return nil
	}
	ci := &sshConnInfo{
		sshUser: strings.TrimSuffix(ctx.User(), forcePasswordSuffix),
		src:     toIPPort(ctx.RemoteAddr()),
		dst:     toIPPort(ctx.LocalAddr()),
	}
	if !tsaddr.IsTailscaleIP(ci.dst.Addr()) {
		return fmt.Errorf("tailssh: rejecting non-Tailscale local address %v", ci.dst)
	}
	if !tsaddr.IsTailscaleIP(ci.src.Addr()) {
		return fmt.Errorf("tailssh: rejecting non-Tailscale remote address %v", ci.src)
	}
	node, uprof, ok := c.srv.lb.WhoIs(ci.src)
	if !ok {
		return fmt.Errorf("unknown Tailscale identity from src %v", ci.src)
	}
	ci.node = node
	ci.uprof = uprof

	c.idH = ctx.SessionID()
	c.info = ci
	c.logf("handling conn: %v", ci.String())
	return nil
}

// evaluatePolicy returns the SSHAction and localUser after evaluating
// the SSHPolicy for this conn. The pubKey may be nil for "none" auth.
func (c *conn) evaluatePolicy(pubKey gossh.PublicKey) (_ *tailcfg.SSHAction, localUser string, _ error) {
	pol, ok := c.sshPolicy()
	if !ok {
		return nil, "", fmt.Errorf("tailssh: rejecting connection; no SSH policy")
	}
	a, localUser, ok := c.evalSSHPolicy(pol, pubKey)
	if !ok {
		return nil, "", fmt.Errorf("tailssh: rejecting connection; no matching policy")
	}
	return a, localUser, nil
}

// pubKeyCacheEntry is the cache value for an HTTPS URL of public keys (like
// "https://github.com/foo.keys")
type pubKeyCacheEntry struct {
	lines []string
	etag  string // if sent by server
	at    time.Time
}

const (
	pubKeyCacheDuration      = time.Minute      // how long to cache non-empty public keys
	pubKeyCacheEmptyDuration = 15 * time.Second // how long to cache empty responses
)

func (srv *server) fetchPublicKeysURLCached(url string) (ce pubKeyCacheEntry, ok bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// Mostly don't care about the size of this cache. Clean rarely.
	if m := srv.fetchPublicKeysCache; len(m) > 50 {
		tooOld := srv.now().Add(pubKeyCacheDuration * 10)
		for k, ce := range m {
			if ce.at.Before(tooOld) {
				delete(m, k)
			}
		}
	}
	ce, ok = srv.fetchPublicKeysCache[url]
	if !ok {
		return ce, false
	}
	maxAge := pubKeyCacheDuration
	if len(ce.lines) == 0 {
		maxAge = pubKeyCacheEmptyDuration
	}
	return ce, srv.now().Sub(ce.at) < maxAge
}

func (srv *server) pubKeyClient() *http.Client {
	if srv.pubKeyHTTPClient != nil {
		return srv.pubKeyHTTPClient
	}
	return http.DefaultClient
}

// fetchPublicKeysURL fetches the public keys from a URL. The strings are in the
// the typical public key "type base64-string [comment]" format seen at e.g.
// https://github.com/USER.keys
func (srv *server) fetchPublicKeysURL(url string) ([]string, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("invalid URL scheme")
	}

	ce, ok := srv.fetchPublicKeysURLCached(url)
	if ok {
		return ce.lines, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if ce.etag != "" {
		req.Header.Add("If-None-Match", ce.etag)
	}
	res, err := srv.pubKeyClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var lines []string
	var etag string
	switch res.StatusCode {
	default:
		err = fmt.Errorf("unexpected status %v", res.Status)
		srv.logf("fetching public keys from %s: %v", url, err)
	case http.StatusNotModified:
		lines = ce.lines
		etag = ce.etag
	case http.StatusOK:
		var all []byte
		all, err = io.ReadAll(io.LimitReader(res.Body, 4<<10))
		if s := strings.TrimSpace(string(all)); s != "" {
			lines = strings.Split(s, "\n")
		}
		etag = res.Header.Get("Etag")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	mak.Set(&srv.fetchPublicKeysCache, url, pubKeyCacheEntry{
		at:    srv.now(),
		lines: lines,
		etag:  etag,
	})
	return lines, err
}

// handleSessionPostSSHAuth runs an SSH session after the SSH-level authentication,
// but not necessarily before all the Tailscale-level extra verification has
// completed. It also handles SFTP requests.
func (c *conn) handleSessionPostSSHAuth(s ssh.Session) {
	// Do this check after auth, but before starting the session.
	switch s.Subsystem() {
	case "sftp", "":
		metricSFTP.Add(1)
	default:
		fmt.Fprintf(s.Stderr(), "Unsupported subsystem %q\r\n", s.Subsystem())
		s.Exit(1)
		return
	}

	ss := c.newSSHSession(s)
	ss.logf("handling new SSH connection from %v (%v) to ssh-user %q", c.info.uprof.LoginName, c.info.src.Addr(), c.localUser.Username)
	ss.logf("access granted to %v as ssh-user %q", c.info.uprof.LoginName, c.localUser.Username)
	ss.run()
}

// resolveNextAction starts at c.currentAction and makes it way through the
// action chain one step at a time. An action without a HoldAndDelegate is
// considered the final action. Once a final action is reached, this function
// will keep returning that action. It updates c.currentAction to the next
// action in the chain. When the final action is reached, it also sets
// c.finalAction to the final action.
func (c *conn) resolveNextAction(sctx ssh.Context) (action *tailcfg.SSHAction, err error) {
	if c.finalAction != nil || c.finalActionErr != nil {
		return c.finalAction, c.finalActionErr
	}

	defer func() {
		if action != nil {
			c.currentAction = action
			if action.Accept || action.Reject {
				c.finalAction = action
			}
		}
		if err != nil {
			c.finalActionErr = err
		}
	}()

	ctx, cancel := context.WithCancel(sctx)
	defer cancel()

	// Loop processing/fetching Actions until one reaches a
	// terminal state (Accept, Reject, or invalid Action), or
	// until fetchSSHAction times out due to the context being
	// done (client disconnect) or its 30 minute timeout passes.
	// (Which is a long time for somebody to see login
	// instructions and go to a URL to do something.)
	action = c.currentAction
	if action.Accept || action.Reject {
		if action.Reject {
			metricTerminalReject.Add(1)
		} else {
			metricTerminalAccept.Add(1)
		}
		return action, nil
	}
	url := action.HoldAndDelegate
	if url == "" {
		metricTerminalMalformed.Add(1)
		return nil, errors.New("reached Action that lacked Accept, Reject, and HoldAndDelegate")
	}
	metricHolds.Add(1)
	url = c.expandDelegateURLLocked(url)
	nextAction, err := c.fetchSSHAction(ctx, url)
	if err != nil {
		metricTerminalFetchError.Add(1)
		return nil, fmt.Errorf("fetching SSHAction from %s: %w", url, err)
	}
	return nextAction, nil
}

func (c *conn) expandDelegateURLLocked(actionURL string) string {
	nm := c.srv.lb.NetMap()
	ci := c.info
	lu := c.localUser
	var dstNodeID string
	if nm != nil {
		dstNodeID = fmt.Sprint(int64(nm.SelfNode.ID))
	}
	return strings.NewReplacer(
		"$SRC_NODE_IP", url.QueryEscape(ci.src.Addr().String()),
		"$SRC_NODE_ID", fmt.Sprint(int64(ci.node.ID)),
		"$DST_NODE_IP", url.QueryEscape(ci.dst.Addr().String()),
		"$DST_NODE_ID", dstNodeID,
		"$SSH_USER", url.QueryEscape(ci.sshUser),
		"$LOCAL_USER", url.QueryEscape(lu.Username),
	).Replace(actionURL)
}

func (c *conn) expandPublicKeyURL(pubKeyURL string) string {
	if !strings.Contains(pubKeyURL, "$") {
		return pubKeyURL
	}
	loginName := c.info.uprof.LoginName
	localPart, _, _ := strings.Cut(loginName, "@")
	return strings.NewReplacer(
		"$LOGINNAME_EMAIL", loginName,
		"$LOGINNAME_LOCALPART", localPart,
	).Replace(pubKeyURL)
}

// sshSession is an accepted Tailscale SSH session.
type sshSession struct {
	ssh.Session
	sharedID string // ID that's shared with control
	logf     logger.Logf

	ctx           context.Context
	cancelCtx     context.CancelCauseFunc
	conn          *conn
	agentListener net.Listener // non-nil if agent-forwarding requested+allowed

	// initialized by launchProcess:
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.Reader // nil for pty sessions
	ptyReq *ssh.Pty  // non-nil for pty sessions

	// We use this sync.Once to ensure that we only terminate the process once,
	// either it exits itself or is terminated
	exitOnce sync.Once
}

func (ss *sshSession) vlogf(format string, args ...any) {
	if sshVerboseLogging() {
		ss.logf(format, args...)
	}
}

func (c *conn) newSSHSession(s ssh.Session) *sshSession {
	sharedID := fmt.Sprintf("sess-%s-%02x", c.srv.now().UTC().Format("20060102T150405"), randBytes(5))
	c.logf("starting session: %v", sharedID)
	ctx, cancel := context.WithCancelCause(s.Context())
	return &sshSession{
		Session:   s,
		sharedID:  sharedID,
		ctx:       ctx,
		cancelCtx: cancel,
		conn:      c,
		logf:      logger.WithPrefix(c.srv.logf, "ssh-session("+sharedID+"): "),
	}
}

// isStillValid reports whether the conn is still valid.
func (c *conn) isStillValid() bool {
	a, localUser, err := c.evaluatePolicy(c.pubKey)
	c.vlogf("stillValid: %+v %v %v", a, localUser, err)
	if err != nil {
		return false
	}
	if !a.Accept && a.HoldAndDelegate == "" {
		return false
	}
	return c.localUser.Username == localUser
}

// checkStillValid checks that the conn is still valid per the latest SSHPolicy.
// If not, it terminates all sessions associated with the conn.
func (c *conn) checkStillValid() {
	if c.isStillValid() {
		return
	}
	metricPolicyChangeKick.Add(1)
	c.logf("session no longer valid per new SSH policy; closing")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		s.cancelCtx(userVisibleError{
			fmt.Sprintf("Access revoked.\r\n"),
			context.Canceled,
		})
	}
}

func (c *conn) fetchSSHAction(ctx context.Context, url string) (*tailcfg.SSHAction, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	bo := backoff.NewBackoff("fetch-ssh-action", c.logf, 10*time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		res, err := c.srv.lb.DoNoiseRequest(req)
		if err != nil {
			bo.BackOff(ctx, err)
			continue
		}
		if res.StatusCode != 200 {
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if len(body) > 1<<10 {
				body = body[:1<<10]
			}
			c.logf("fetch of %v: %s, %s", url, res.Status, body)
			bo.BackOff(ctx, fmt.Errorf("unexpected status: %v", res.Status))
			continue
		}
		a := new(tailcfg.SSHAction)
		err = json.NewDecoder(res.Body).Decode(a)
		res.Body.Close()
		if err != nil {
			c.logf("invalid next SSHAction JSON from %v: %v", url, err)
			bo.BackOff(ctx, err)
			continue
		}
		return a, nil
	}
}

// killProcessOnContextDone waits for ss.ctx to be done and kills the process,
// unless the process has already exited.
func (ss *sshSession) killProcessOnContextDone() {
	<-ss.ctx.Done()
	// Either the process has already exited, in which case this does nothing.
	// Or, the process is still running in which case this will kill it.
	ss.exitOnce.Do(func() {
		err := context.Cause(ss.ctx)
		if serr, ok := err.(SSHTerminationError); ok {
			msg := serr.SSHTerminationMessage()
			if msg != "" {
				io.WriteString(ss.Stderr(), "\r\n\r\n"+msg+"\r\n\r\n")
			}
		}
		ss.logf("terminating SSH session from %v: %v", ss.conn.info.src.Addr(), err)
		// We don't need to Process.Wait here, sshSession.run() does
		// the waiting regardless of termination reason.

		// TODO(maisem): should this be a SIGTERM followed by a SIGKILL?
		ss.cmd.Process.Kill()
	})
}

// attachSession registers ss as an active session.
func (c *conn) attachSession(ss *sshSession) {
	c.srv.sessionWaitGroup.Add(1)
	if ss.sharedID == "" {
		panic("empty sharedID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = append(c.sessions, ss)
}

// detachSession unregisters s from the list of active sessions.
func (c *conn) detachSession(ss *sshSession) {
	defer c.srv.sessionWaitGroup.Done()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.sessions {
		if s == ss {
			c.sessions = append(c.sessions[:i], c.sessions[i+1:]...)
			break
		}
	}
}

var errSessionDone = errors.New("session is done")

// handleSSHAgentForwarding starts a Unix socket listener and in the background
// forwards agent connections between the listener and the ssh.Session.
// On success, it assigns ss.agentListener.
func (ss *sshSession) handleSSHAgentForwarding(s ssh.Session, lu *user.User) error {
	if !ssh.AgentRequested(ss) || !ss.conn.finalAction.AllowAgentForwarding {
		return nil
	}
	ss.logf("ssh: agent forwarding requested")
	ln, err := ssh.NewAgentListener()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && ln != nil {
			ln.Close()
		}
	}()

	uid, err := strconv.ParseUint(lu.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.ParseUint(lu.Gid, 10, 32)
	if err != nil {
		return err
	}
	socket := ln.Addr().String()
	dir := filepath.Dir(socket)
	// Make sure the socket is accessible only by the user.
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	if err := os.Chown(socket, int(uid), int(gid)); err != nil {
		return err
	}
	// Make sure the dir is also accessible.
	if err := os.Chmod(dir, 0755); err != nil {
		return err
	}

	go ssh.ForwardAgentConnections(ln, s)
	ss.agentListener = ln
	return nil
}

// run is the entrypoint for a newly accepted SSH session.
//
// It handles ss once it's been accepted and determined
// that it should run.
func (ss *sshSession) run() {
	metricActiveSessions.Add(1)
	defer metricActiveSessions.Add(-1)
	defer ss.cancelCtx(errSessionDone)

	if attached := ss.conn.srv.attachSessionToConnIfNotShutdown(ss); !attached {
		fmt.Fprintf(ss, "Tailscale SSH is shutting down\r\n")
		ss.Exit(1)
		return
	}
	defer ss.conn.detachSession(ss)

	lu := ss.conn.localUser
	logf := ss.logf

	if ss.conn.finalAction.SessionDuration != 0 {
		t := time.AfterFunc(ss.conn.finalAction.SessionDuration, func() {
			ss.cancelCtx(userVisibleError{
				fmt.Sprintf("Session timeout of %v elapsed.", ss.conn.finalAction.SessionDuration),
				context.DeadlineExceeded,
			})
		})
		defer t.Stop()
	}

	if euid := os.Geteuid(); euid != 0 {
		if lu.Uid != fmt.Sprint(euid) {
			ss.logf("can't switch to user %q from process euid %v", lu.Username, euid)
			fmt.Fprintf(ss, "can't switch user\r\n")
			ss.Exit(1)
			return
		}
	}

	// Take control of the PTY so that we can configure it below.
	// See https://github.com/tailscale/tailscale/issues/4146
	ss.DisablePTYEmulation()

	var rec *recording // or nil if disabled
	if ss.Subsystem() != "sftp" {
		if err := ss.handleSSHAgentForwarding(ss, lu); err != nil {
			ss.logf("agent forwarding failed: %v", err)
		} else if ss.agentListener != nil {
			// TODO(maisem/bradfitz): add a way to close all session resources
			defer ss.agentListener.Close()
		}

		if ss.shouldRecord() {
			var err error
			rec, err = ss.startNewRecording()
			if err != nil {
				var uve userVisibleError
				if errors.As(err, &uve) {
					fmt.Fprintf(ss, "%s\r\n", uve.SSHTerminationMessage())
				} else {
					fmt.Fprintf(ss, "can't start new recording\r\n")
				}
				ss.logf("startNewRecording: %v", err)
				ss.Exit(1)
				return
			}
			if rec != nil {
				defer rec.Close()
			}
		}
	}

	err := ss.launchProcess()
	if err != nil {
		logf("start failed: %v", err.Error())
		if errors.Is(err, context.Canceled) {
			err := context.Cause(ss.ctx)
			var uve userVisibleError
			if errors.As(err, &uve) {
				fmt.Fprintf(ss, "%s\r\n", uve)
			}
		}
		ss.Exit(1)
		return
	}
	go ss.killProcessOnContextDone()

	go func() {
		defer ss.stdin.Close()
		if _, err := io.Copy(rec.writer("i", ss.stdin), ss); err != nil {
			logf("stdin copy: %v", err)
			ss.cancelCtx(err)
		}
	}()
	var openOutputStreams atomic.Int32
	if ss.stderr != nil {
		openOutputStreams.Store(2)
	} else {
		openOutputStreams.Store(1)
	}
	go func() {
		defer ss.stdout.Close()
		_, err := io.Copy(rec.writer("o", ss), ss.stdout)
		if err != nil && !errors.Is(err, io.EOF) {
			logf("stdout copy: %v", err)
			ss.cancelCtx(err)
		}
		if openOutputStreams.Add(-1) == 0 {
			ss.CloseWrite()
		}
	}()
	// stderr is nil for ptys.
	if ss.stderr != nil {
		go func() {
			_, err := io.Copy(ss.Stderr(), ss.stderr)
			if err != nil {
				logf("stderr copy: %v", err)
			}
			if openOutputStreams.Add(-1) == 0 {
				ss.CloseWrite()
			}
		}()
	}

	err = ss.cmd.Wait()
	// This will either make the SSH Termination goroutine be a no-op,
	// or itself will be a no-op because the process was killed by the
	// aforementioned goroutine.
	ss.exitOnce.Do(func() {})

	if err == nil {
		ss.logf("Session complete")
		ss.Exit(0)
		return
	}
	if ee, ok := err.(*exec.ExitError); ok {
		code := ee.ProcessState.ExitCode()
		ss.logf("Wait: code=%v", code)
		ss.Exit(code)
		return
	}

	ss.logf("Wait: %v", err)
	ss.Exit(1)
	return
}

// recordSSHToLocalDisk is a deprecated dev knob to allow recording SSH sessions
// to local storage. It is only used if there is no recording configured by the
// coordination server. This will be removed in the future.
var recordSSHToLocalDisk = envknob.RegisterBool("TS_DEBUG_LOG_SSH")

// recorders returns the list of recorders to use for this session.
// If the final action has a non-empty list of recorders, that list is
// returned. Otherwise, the list of recorders from the initial action
// is returned.
func (ss *sshSession) recorders() ([]netip.AddrPort, *tailcfg.SSHRecorderFailureAction) {
	if len(ss.conn.finalAction.Recorders) > 0 {
		return ss.conn.finalAction.Recorders, ss.conn.finalAction.OnRecordingFailure
	}
	return ss.conn.action0.Recorders, ss.conn.action0.OnRecordingFailure
}

func (ss *sshSession) shouldRecord() bool {
	recs, _ := ss.recorders()
	return len(recs) > 0 || recordSSHToLocalDisk()
}

type sshConnInfo struct {
	// sshUser is the requested local SSH username ("root", "alice", etc).
	sshUser string

	// src is the Tailscale IP and port that the connection came from.
	src netip.AddrPort

	// dst is the Tailscale IP and port that the connection came for.
	dst netip.AddrPort

	// node is srcIP's node.
	node *tailcfg.Node

	// uprof is node's UserProfile.
	uprof tailcfg.UserProfile
}

func (ci *sshConnInfo) String() string {
	return fmt.Sprintf("%v->%v@%v", ci.src, ci.sshUser, ci.dst)
}

func (c *conn) ruleExpired(r *tailcfg.SSHRule) bool {
	if r.RuleExpires == nil {
		return false
	}
	return r.RuleExpires.Before(c.srv.now())
}

func (c *conn) evalSSHPolicy(pol *tailcfg.SSHPolicy, pubKey gossh.PublicKey) (a *tailcfg.SSHAction, localUser string, ok bool) {
	for _, r := range pol.Rules {
		if a, localUser, err := c.matchRule(r, pubKey); err == nil {
			return a, localUser, true
		}
	}
	return nil, "", false
}

// internal errors for testing; they don't escape to callers or logs.
var (
	errNilRule        = errors.New("nil rule")
	errNilAction      = errors.New("nil action")
	errRuleExpired    = errors.New("rule expired")
	errPrincipalMatch = errors.New("principal didn't match")
	errUserMatch      = errors.New("user didn't match")
	errInvalidConn    = errors.New("invalid connection state")
)

func (c *conn) matchRule(r *tailcfg.SSHRule, pubKey gossh.PublicKey) (a *tailcfg.SSHAction, localUser string, err error) {
	defer func() {
		c.vlogf("matchRule(%+v): %v", r, err)
	}()

	if c == nil {
		return nil, "", errInvalidConn
	}
	if c.info == nil {
		c.logf("invalid connection state")
		return nil, "", errInvalidConn
	}
	if r == nil {
		return nil, "", errNilRule
	}
	if r.Action == nil {
		return nil, "", errNilAction
	}
	if c.ruleExpired(r) {
		return nil, "", errRuleExpired
	}
	if !r.Action.Reject {
		// For all but Reject rules, SSHUsers is required.
		// If SSHUsers is nil or empty, mapLocalUser will return an
		// empty string anyway.
		localUser = mapLocalUser(r.SSHUsers, c.info.sshUser)
		if localUser == "" {
			return nil, "", errUserMatch
		}
	}
	if ok, err := c.anyPrincipalMatches(r.Principals, pubKey); err != nil {
		return nil, "", err
	} else if !ok {
		return nil, "", errPrincipalMatch
	}
	return r.Action, localUser, nil
}

func mapLocalUser(ruleSSHUsers map[string]string, reqSSHUser string) (localUser string) {
	v, ok := ruleSSHUsers[reqSSHUser]
	if !ok {
		v = ruleSSHUsers["*"]
	}
	if v == "=" {
		return reqSSHUser
	}
	return v
}

func (c *conn) anyPrincipalMatches(ps []*tailcfg.SSHPrincipal, pubKey gossh.PublicKey) (bool, error) {
	for _, p := range ps {
		if p == nil {
			continue
		}
		if ok, err := c.principalMatches(p, pubKey); err != nil {
			return false, err
		} else if ok {
			return true, nil
		}
	}
	return false, nil
}

func (c *conn) principalMatches(p *tailcfg.SSHPrincipal, pubKey gossh.PublicKey) (bool, error) {
	if !c.principalMatchesTailscaleIdentity(p) {
		return false, nil
	}
	return c.principalMatchesPubKey(p, pubKey)
}

// principalMatchesTailscaleIdentity reports whether one of p's four fields
// that match the Tailscale identity match (Node, NodeIP, UserLogin, Any).
// This function does not consider PubKeys.
func (c *conn) principalMatchesTailscaleIdentity(p *tailcfg.SSHPrincipal) bool {
	ci := c.info
	if p.Any {
		return true
	}
	if !p.Node.IsZero() && ci.node != nil && p.Node == ci.node.StableID {
		return true
	}
	if p.NodeIP != "" {
		if ip, _ := netip.ParseAddr(p.NodeIP); ip == ci.src.Addr() {
			return true
		}
	}
	if p.UserLogin != "" && ci.uprof.LoginName == p.UserLogin {
		return true
	}
	return false
}

func (c *conn) principalMatchesPubKey(p *tailcfg.SSHPrincipal, clientPubKey gossh.PublicKey) (bool, error) {
	if len(p.PubKeys) == 0 {
		return true, nil
	}
	if clientPubKey == nil {
		return false, nil
	}
	knownKeys := p.PubKeys
	if len(knownKeys) == 1 && strings.HasPrefix(knownKeys[0], "https://") {
		var err error
		knownKeys, err = c.srv.fetchPublicKeysURL(c.expandPublicKeyURL(knownKeys[0]))
		if err != nil {
			return false, err
		}
	}
	for _, knownKey := range knownKeys {
		if pubKeyMatchesAuthorizedKey(clientPubKey, knownKey) {
			return true, nil
		}
	}
	return false, nil
}

func pubKeyMatchesAuthorizedKey(pubKey ssh.PublicKey, wantKey string) bool {
	wantKeyType, rest, ok := strings.Cut(wantKey, " ")
	if !ok {
		return false
	}
	if pubKey.Type() != wantKeyType {
		return false
	}
	wantKeyB64, _, _ := strings.Cut(rest, " ")
	wantKeyData, _ := base64.StdEncoding.DecodeString(wantKeyB64)
	return len(wantKeyData) > 0 && bytes.Equal(pubKey.Marshal(), wantKeyData)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// CastHeader is the header of an asciinema file.
type CastHeader struct {
	// Version is the asciinema file format version.
	Version int `json:"version"`

	// Width is the terminal width in characters.
	// It is non-zero for Pty sessions.
	Width int `json:"width"`

	// Height is the terminal height in characters.
	// It is non-zero for Pty sessions.
	Height int `json:"height"`

	// Timestamp is the unix timestamp of when the recording started.
	Timestamp int64 `json:"timestamp"`

	// Env is the environment variables of the session.
	// Only "TERM" is set (2023-03-22).
	Env map[string]string `json:"env"`

	// Command is the command that was executed.
	// Typically empty for shell sessions.
	Command string `json:"command,omitempty"`

	// Tailscale-specific fields:
	// SrcNode is the FQDN of the node originating the connection.
	// It is also the MagicDNS name for the node.
	// It does not have a trailing dot.
	// e.g. "host.tail-scale.ts.net"
	SrcNode string `json:"srcNode"`

	// SrcNodeID is the node ID of the node originating the connection.
	SrcNodeID tailcfg.StableNodeID `json:"srcNodeID"`

	// SrcNodeTags is the list of tags on the node originating the connection (if any).
	SrcNodeTags []string `json:"srcNodeTags,omitempty"`

	// SrcNodeUserID is the user ID of the node originating the connection (if not tagged).
	SrcNodeUserID tailcfg.UserID `json:"srcNodeUserID,omitempty"` // if not tagged

	// SrcNodeUser is the LoginName of the node originating the connection (if not tagged).
	SrcNodeUser string `json:"srcNodeUser,omitempty"`

	// SSHUser is the username as presented by the client.
	SSHUser string `json:"sshUser"` // as presented by the client

	// LocalUser is the effective username on the server.
	LocalUser string `json:"localUser"`

	// ConnectionID uniquely identifies a connection made to the SSH server.
	// It may be shared across multiple sessions over the same connection in
	// case of SSH multiplexing.
	ConnectionID string `json:"connectionID"`
}

// sessionRecordingClient returns an http.Client that uses srv.lb.Dialer() to
// dial connections. This is used to make requests to the session recording
// server to upload session recordings.
// It uses the provided dialCtx to dial connections, and limits a single dial
// to 5 seconds.
func (ss *sshSession) sessionRecordingClient(dialCtx context.Context) (*http.Client, error) {
	dialer := ss.conn.srv.lb.Dialer()
	if dialer == nil {
		return nil, errors.New("no peer API transport")
	}
	tr := dialer.PeerAPITransport().Clone()
	dialContextFn := tr.DialContext

	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		perAttemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		go func() {
			select {
			case <-perAttemptCtx.Done():
			case <-dialCtx.Done():
				cancel()
			}
		}()
		return dialContextFn(perAttemptCtx, network, addr)
	}
	return &http.Client{
		Transport: tr,
	}, nil
}

// connectToRecorder connects to the recorder at any of the provided addresses.
// It returns the first successful response, or a multierr if all attempts fail.
//
// On success, it returns a WriteCloser that can be used to upload the
// recording, and a channel that will be sent an error (or nil) when the upload
// fails or completes.
//
// In both cases, a slice of SSHRecordingAttempts is returned which detail the
// attempted recorder IP and the error message, if the attempt failed. The
// attempts are in order the recorder(s) was attempted. If successful a
// successful connection is made, the last attempt in the slice is the
// attempt for connected recorder.
func (ss *sshSession) connectToRecorder(ctx context.Context, recs []netip.AddrPort) (io.WriteCloser, []*tailcfg.SSHRecordingAttempt, <-chan error, error) {
	if len(recs) == 0 {
		return nil, nil, nil, errors.New("no recorders configured")
	}
	// We use a special context for dialing the recorder, so that we can
	// limit the time we spend dialing to 30 seconds and still have an
	// unbounded context for the upload.
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel()
	hc, err := ss.sessionRecordingClient(dialCtx)
	if err != nil {
		return nil, nil, nil, err
	}

	var errs []error
	var attempts []*tailcfg.SSHRecordingAttempt
	for _, ap := range recs {
		attempt := &tailcfg.SSHRecordingAttempt{
			Recorder: ap,
		}
		attempts = append(attempts, attempt)

		// We dial the recorder and wait for it to send a 100-continue
		// response before returning from this function. This ensures that
		// the recorder is ready to accept the recording.

		// got100 is closed when we receive the 100-continue response.
		got100 := make(chan struct{})
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			Got100Continue: func() {
				close(got100)
			},
		})

		pr, pw := io.Pipe()
		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://%s:%d/record", ap.Addr(), ap.Port()), pr)
		if err != nil {
			err = fmt.Errorf("recording: error starting recording: %w", err)
			attempt.FailureMessage = err.Error()
			errs = append(errs, err)
			continue
		}
		// We set the Expect header to 100-continue, so that the recorder
		// will send a 100-continue response before it starts reading the
		// request body.
		req.Header.Set("Expect", "100-continue")

		// errChan is used to indicate the result of the request.
		errChan := make(chan error, 1)
		go func() {
			resp, err := hc.Do(req)
			if err != nil {
				errChan <- fmt.Errorf("recording: error starting recording: %w", err)
				return
			}
			if resp.StatusCode != 200 {
				errChan <- fmt.Errorf("recording: unexpected status: %v", resp.Status)
				return
			}
			errChan <- nil
		}()
		select {
		case <-got100:
		case err := <-errChan:
			// If we get an error before we get the 100-continue response,
			// we need to try another recorder.
			if err == nil {
				// If the error is nil, we got a 200 response, which
				// is unexpected as we haven't sent any data yet.
				err = errors.New("recording: unexpected EOF")
			}
			attempt.FailureMessage = err.Error()
			errs = append(errs, err)
			continue
		}
		return pw, attempts, errChan, nil
	}
	return nil, attempts, nil, multierr.New(errs...)
}

func (ss *sshSession) openFileForRecording(now time.Time) (_ io.WriteCloser, err error) {
	varRoot := ss.conn.srv.lb.TailscaleVarRoot()
	if varRoot == "" {
		return nil, errors.New("no var root for recording storage")
	}
	dir := filepath.Join(varRoot, "ssh-sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("ssh-session-%v-*.cast", now.UnixNano()))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// startNewRecording starts a new SSH session recording.
// It may return a nil recording if recording is not available.
func (ss *sshSession) startNewRecording() (_ *recording, err error) {
	// We store the node key as soon as possible when creating
	// a new recording incase of FUS.
	nodeKey := ss.conn.srv.lb.NodeKey()
	if nodeKey.IsZero() {
		return nil, errors.New("ssh server is unavailable: no node key")
	}

	recorders, onFailure := ss.recorders()
	var localRecording bool
	if len(recorders) == 0 {
		if recordSSHToLocalDisk() {
			localRecording = true
		} else {
			return nil, errors.New("no recorders configured")
		}
	}

	var w ssh.Window
	if ptyReq, _, isPtyReq := ss.Pty(); isPtyReq {
		w = ptyReq.Window
	}

	term := envValFromList(ss.Environ(), "TERM")
	if term == "" {
		term = "xterm-256color" // something non-empty
	}

	now := time.Now()
	rec := &recording{
		ss:       ss,
		start:    now,
		failOpen: onFailure == nil || onFailure.TerminateSessionWithMessage == "",
	}

	// We want to use a background context for uploading and not ss.ctx.
	// ss.ctx is closed when the session closes, but we don't want to break the upload at that time.
	// Instead we want to wait for the session to close the writer when it finishes.
	ctx := context.Background()
	if localRecording {
		rec.out, err = ss.openFileForRecording(now)
		if err != nil {
			return nil, err
		}
	} else {
		var errChan <-chan error
		var attempts []*tailcfg.SSHRecordingAttempt
		rec.out, attempts, errChan, err = ss.connectToRecorder(ctx, recorders)
		if err != nil {
			if onFailure != nil && onFailure.NotifyURL != "" && len(attempts) > 0 {
				ss.notifyControl(ctx, nodeKey, tailcfg.SSHSessionRecordingRejected, attempts, onFailure.NotifyURL)
			}

			if onFailure != nil && onFailure.RejectSessionWithMessage != "" {
				ss.logf("recording: error starting recording (rejecting session): %v", err)
				return nil, userVisibleError{
					error: err,
					msg:   onFailure.RejectSessionWithMessage,
				}
			}
			ss.logf("recording: error starting recording (failing open): %v", err)
			return nil, nil
		}
		go func() {
			err := <-errChan
			if err == nil {
				// Success.
				return
			}
			if onFailure != nil && onFailure.NotifyURL != "" && len(attempts) > 0 {
				lastAttempt := attempts[len(attempts)-1]
				lastAttempt.FailureMessage = err.Error()

				ss.notifyControl(ctx, nodeKey, tailcfg.SSHSessionRecordingTerminated, attempts, onFailure.NotifyURL)
			}
			if onFailure != nil && onFailure.TerminateSessionWithMessage != "" {
				ss.logf("recording: error uploading recording (closing session): %v", err)
				ss.cancelCtx(userVisibleError{
					error: err,
					msg:   onFailure.TerminateSessionWithMessage,
				})
				return
			}
			ss.logf("recording: error uploading recording (failing open): %v", err)
		}()
	}

	ch := CastHeader{
		Version:   2,
		Width:     w.Width,
		Height:    w.Height,
		Timestamp: now.Unix(),
		Command:   strings.Join(ss.Command(), " "),
		Env: map[string]string{
			"TERM": term,
			// TODO(bradfitz): anything else important?
			// including all seems noisey, but maybe we should
			// for auditing. But first need to break
			// launchProcess's startWithStdPipes and
			// startWithPTY up so that they first return the cmd
			// without starting it, and then a step that starts
			// it. Then we can (1) make the cmd, (2) start the
			// recording, (3) start the process.
		},
		SSHUser:      ss.conn.info.sshUser,
		LocalUser:    ss.conn.localUser.Username,
		SrcNode:      strings.TrimSuffix(ss.conn.info.node.Name, "."),
		SrcNodeID:    ss.conn.info.node.StableID,
		ConnectionID: ss.conn.connID,
	}
	if !ss.conn.info.node.IsTagged() {
		ch.SrcNodeUser = ss.conn.info.uprof.LoginName
		ch.SrcNodeUserID = ss.conn.info.node.User
	} else {
		ch.SrcNodeTags = ss.conn.info.node.Tags
	}
	j, err := json.Marshal(ch)
	if err != nil {
		return nil, err
	}
	j = append(j, '\n')
	if _, err := rec.out.Write(j); err != nil {
		if errors.Is(err, io.ErrClosedPipe) && ss.ctx.Err() != nil {
			// If we got an io.ErrClosedPipe, it's likely because
			// the recording server closed the connection on us. Return
			// the original context error instead.
			return nil, context.Cause(ss.ctx)
		}
		return nil, err
	}
	return rec, nil
}

// notifyControl sends a SSHEventNotifyRequest to control over noise.
// A SSHEventNotifyRequest is sent when an action or state reached during
// an SSH session is a defined EventType.
func (ss *sshSession) notifyControl(ctx context.Context, nodeKey key.NodePublic, notifyType tailcfg.SSHEventType, attempts []*tailcfg.SSHRecordingAttempt, url string) {
	re := tailcfg.SSHEventNotifyRequest{
		EventType:         notifyType,
		ConnectionID:      ss.conn.connID,
		CapVersion:        tailcfg.CurrentCapabilityVersion,
		NodeKey:           nodeKey,
		SrcNode:           ss.conn.info.node.ID,
		SSHUser:           ss.conn.info.sshUser,
		LocalUser:         ss.conn.localUser.Username,
		RecordingAttempts: attempts,
	}

	body, err := json.Marshal(re)
	if err != nil {
		ss.logf("notifyControl: unable to marshal SSHNotifyRequest:", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		ss.logf("notifyControl: unable to create request:", err)
		return
	}

	resp, err := ss.conn.srv.lb.DoNoiseRequest(req)
	if err != nil {
		ss.logf("notifyControl: unable to send noise request:", err)
		return
	}

	if resp.StatusCode != http.StatusCreated {
		ss.logf("notifyControl: noise request returned status code %v", resp.StatusCode)
		return
	}
}

// recording is the state for an SSH session recording.
type recording struct {
	ss    *sshSession
	start time.Time

	// failOpen specifies whether the session should be allowed to
	// continue if writing to the recording fails.
	failOpen bool

	mu  sync.Mutex // guards writes to, close of out
	out io.WriteCloser
}

func (r *recording) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		return nil
	}
	err := r.out.Close()
	r.out = nil
	return err
}

// writer returns an io.Writer around w that first records the write.
//
// The dir should be "i" for input or "o" for output.
//
// If r is nil, it returns w unchanged.
//
// Currently (2023-03-21) we only record output, not input.
func (r *recording) writer(dir string, w io.Writer) io.Writer {
	if r == nil {
		return w
	}
	if dir == "i" {
		// TODO: record input? Maybe not, since it might contain
		// passwords.
		return w
	}
	return &loggingWriter{r: r, dir: dir, w: w}
}

// loggingWriter is an io.Writer wrapper that writes first an
// asciinema JSON cast format recording line, and then writes to w.
type loggingWriter struct {
	r   *recording
	dir string    // "i" or "o" (input or output)
	w   io.Writer // underlying Writer, after writing to r.out

	// recordingFailedOpen specifies whether we've failed to write to
	// r.out and should stop trying. It is set to true if we fail to write
	// to r.out and r.failOpen is set.
	recordingFailedOpen bool
}

func (w *loggingWriter) Write(p []byte) (n int, err error) {
	if !w.recordingFailedOpen {
		j, err := json.Marshal([]any{
			time.Since(w.r.start).Seconds(),
			w.dir,
			string(p),
		})
		if err != nil {
			return 0, err
		}
		j = append(j, '\n')
		if err := w.writeCastLine(j); err != nil {
			if !w.r.failOpen {
				return 0, err
			}
			w.recordingFailedOpen = true
		}
	}
	return w.w.Write(p)
}

func (w loggingWriter) writeCastLine(j []byte) error {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	if w.r.out == nil {
		return errors.New("logger closed")
	}
	_, err := w.r.out.Write(j)
	if err != nil {
		return fmt.Errorf("logger Write: %w", err)
	}
	return nil
}

func envValFromList(env []string, wantKey string) (v string) {
	for _, kv := range env {
		if thisKey, v, ok := strings.Cut(kv, "="); ok && envEq(thisKey, wantKey) {
			return v
		}
	}
	return ""
}

// envEq reports whether environment variable a == b for the current
// operating system.
func envEq(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

var (
	metricActiveSessions       = clientmetric.NewGauge("ssh_active_sessions")
	metricIncomingConnections  = clientmetric.NewCounter("ssh_incoming_connections")
	metricPublicKeyConnections = clientmetric.NewCounter("ssh_publickey_connections") // total
	metricPublicKeyAccepts     = clientmetric.NewCounter("ssh_publickey_accepts")     // accepted subset of ssh_publickey_connections
	metricTerminalAccept       = clientmetric.NewCounter("ssh_terminalaction_accept")
	metricTerminalReject       = clientmetric.NewCounter("ssh_terminalaction_reject")
	metricTerminalInterrupt    = clientmetric.NewCounter("ssh_terminalaction_interrupt")
	metricTerminalMalformed    = clientmetric.NewCounter("ssh_terminalaction_malformed")
	metricTerminalFetchError   = clientmetric.NewCounter("ssh_terminalaction_fetch_error")
	metricHolds                = clientmetric.NewCounter("ssh_holds")
	metricPolicyChangeKick     = clientmetric.NewCounter("ssh_policy_change_kick")
	metricSFTP                 = clientmetric.NewCounter("ssh_sftp_requests")
	metricLocalPortForward     = clientmetric.NewCounter("ssh_local_port_forward_requests")
)

// userVisibleError is a wrapper around an error that implements
// SSHTerminationError, so msg is written to their session.
type userVisibleError struct {
	msg string
	error
}

func (ue userVisibleError) SSHTerminationMessage() string { return ue.msg }

// SSHTerminationError is implemented by errors that terminate an SSH
// session and should be written to user's sessions.
type SSHTerminationError interface {
	error
	SSHTerminationMessage() string
}
