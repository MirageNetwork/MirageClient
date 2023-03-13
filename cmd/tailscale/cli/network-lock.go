// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tka"
	"tailscale.com/types/key"
)

var netlockCmd = &ffcli.Command{
	Name:       "lock",
	ShortUsage: "lock <sub-command> <arguments>",
	ShortHelp:  "Manage miragenet lock",
	LongHelp:   "Manage miragenet lock",
	Subcommands: []*ffcli.Command{
		nlInitCmd,
		nlStatusCmd,
		nlAddCmd,
		nlRemoveCmd,
		nlSignCmd,
		nlDisableCmd,
		nlDisablementKDFCmd,
		nlLogCmd,
		nlLocalDisableCmd,
		nlTskeyWrapCmd,
	},
	Exec: runNetworkLockStatus,
}

var nlInitArgs struct {
	numDisablements       int
	disablementForSupport bool
	confirm               bool
}

var nlInitCmd = &ffcli.Command{
	Name:       "init",
	ShortUsage: "init [--gen-disablement-for-support] --gen-disablements N <trusted-key>...",
	ShortHelp:  "Initialize miragenet lock",
	LongHelp: strings.TrimSpace(`

The 'mirage lock init' command initializes miragenet lock for the
entire miragenet. The miragenet lock keys specified are those initially
trusted to sign nodes or to make further changes to miragenet lock.

You can identify the miragenet lock key for a node you wish to trust by
running 'mirage lock' on that node, and copying the node's miragenet
lock key.

To disable miragenet lock, use the 'mirage lock disable' command
along with one of the disablement secrets.
The number of disablement secrets to be generated is specified using the
--gen-disablements flag. Initializing miragenet lock requires at least
one disablement.

If --gen-disablement-for-support is specified, an additional disablement secret
will be generated and transmitted to Mirage, which support can use to disable
miragenet lock. We recommend setting this flag.

`),
	Exec: runNetworkLockInit,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("lock init")
		fs.IntVar(&nlInitArgs.numDisablements, "gen-disablements", 1, "number of disablement secrets to generate")
		fs.BoolVar(&nlInitArgs.disablementForSupport, "gen-disablement-for-support", false, "generates and transmits a disablement secret for Mirage support")
		fs.BoolVar(&nlInitArgs.confirm, "confirm", false, "do not prompt for confirmation")
		return fs
	})(),
}

func runNetworkLockInit(ctx context.Context, args []string) error {
	st, err := localClient.NetworkLockStatus(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}
	if st.Enabled {
		return errors.New("miragenet lock is already enabled")
	}

	// Parse initially-trusted keys & disablement values.
	keys, disablementValues, err := parseNLArgs(args, true, true)
	if err != nil {
		return err
	}

	// Common mistake: Not specifying the current node's key as one of the trusted keys.
	foundSelfKey := false
	for _, k := range keys {
		keyID, err := k.ID()
		if err != nil {
			return err
		}
		if bytes.Equal(keyID, st.PublicKey.KeyID()) {
			foundSelfKey = true
			break
		}
	}
	if !foundSelfKey {
		return errors.New("the miragenet lock key of the current node must be one of the trusted keys during initialization")
	}

	fmt.Println("You are initializing miragenet lock with the following trusted signing keys:")
	for _, k := range keys {
		fmt.Printf(" - tlpub:%x (%s key)\n", k.Public, k.Kind.String())
	}
	fmt.Println()

	if !nlInitArgs.confirm {
		fmt.Printf("%d disablement secrets will be generated.\n", nlInitArgs.numDisablements)
		if nlInitArgs.disablementForSupport {
			fmt.Println("A disablement secret will be generated and transmitted to Mirage support.")
		}

		genSupportFlag := ""
		if nlInitArgs.disablementForSupport {
			genSupportFlag = "--gen-disablement-for-support "
		}
		fmt.Println("\nIf this is correct, please re-run this command with the --confirm flag:")
		fmt.Printf("\t%s lock init --confirm --gen-disablements %d %s%s", os.Args[0], nlInitArgs.numDisablements, genSupportFlag, strings.Join(args, " "))
		fmt.Println()
		return nil
	}

	fmt.Printf("%d disablement secrets have been generated and are printed below. Take note of them now, they WILL NOT be shown again.\n", nlInitArgs.numDisablements)
	for i := 0; i < nlInitArgs.numDisablements; i++ {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		fmt.Printf("\tdisablement-secret:%X\n", secret[:])
		disablementValues = append(disablementValues, tka.DisablementKDF(secret[:]))
	}

	var supportDisablement []byte
	if nlInitArgs.disablementForSupport {
		supportDisablement = make([]byte, 32)
		if _, err := rand.Read(supportDisablement); err != nil {
			return err
		}
		disablementValues = append(disablementValues, tka.DisablementKDF(supportDisablement))
		fmt.Println("A disablement secret for Mirage support has been generated and will be transmitted to Mirage upon initialization.")
	}

	// The state returned by NetworkLockInit likely doesn't contain the initialized state,
	// because that has to tick through from netmaps.
	if _, err := localClient.NetworkLockInit(ctx, keys, disablementValues, supportDisablement); err != nil {
		return err
	}

	fmt.Println("Initialization complete.")
	return nil
}

var nlStatusArgs struct {
	json bool
}

var nlStatusCmd = &ffcli.Command{
	Name:       "status",
	ShortUsage: "status",
	ShortHelp:  "Outputs the state of miragenet lock",
	LongHelp:   "Outputs the state of miragenet lock",
	Exec:       runNetworkLockStatus,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("lock status")
		fs.BoolVar(&nlStatusArgs.json, "json", false, "output in JSON format (WARNING: format subject to change)")
		return fs
	})(),
}

func runNetworkLockStatus(ctx context.Context, args []string) error {
	st, err := localClient.NetworkLockStatus(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}

	if nlStatusArgs.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}

	if st.Enabled {
		fmt.Println("Miragenet lock is ENABLED.")
	} else {
		fmt.Println("Miragenet lock is NOT enabled.")
	}
	fmt.Println()

	if st.Enabled && st.NodeKey != nil && !st.PublicKey.IsZero() {
		if st.NodeKeySigned {
			fmt.Println("This node is accessible under miragenet lock.")
		} else {
			fmt.Println("This node is LOCKED OUT by miragenet-lock, and action is required to establish connectivity.")
			fmt.Printf("Run the following command on a node with a trusted key:\n\tmirage lock sign %v %s\n", st.NodeKey, st.PublicKey.CLIString())
		}
		fmt.Println()
	}

	if !st.PublicKey.IsZero() {
		fmt.Printf("This node's miragenet-lock key: %s\n", st.PublicKey.CLIString())
		fmt.Println()
	}

	if st.Enabled && len(st.TrustedKeys) > 0 {
		fmt.Println("Trusted signing keys:")
		for _, k := range st.TrustedKeys {
			var line strings.Builder
			line.WriteString("\t")
			line.WriteString(k.Key.CLIString())
			line.WriteString("\t")
			line.WriteString(fmt.Sprint(k.Votes))
			line.WriteString("\t")
			if k.Key == st.PublicKey {
				line.WriteString("(self)")
			}
			if k.Metadata["purpose"] == "pre-auth key" {
				if preauthKeyID := k.Metadata["authkey_stableid"]; preauthKeyID != "" {
					line.WriteString("(pre-auth key ")
					line.WriteString(preauthKeyID)
					line.WriteString(")")
				} else {
					line.WriteString("(pre-auth key)")
				}
			}
			fmt.Println(line.String())
		}
	}

	if st.Enabled && len(st.FilteredPeers) > 0 {
		fmt.Println()
		fmt.Println("The following nodes are locked out by miragenet lock and cannot connect to other nodes:")
		for _, p := range st.FilteredPeers {
			var line strings.Builder
			line.WriteString("\t")
			line.WriteString(p.Name)
			line.WriteString("\t")
			for i, addr := range p.TailscaleIPs {
				line.WriteString(addr.String())
				if i < len(p.TailscaleIPs)-1 {
					line.WriteString(",")
				}
			}
			line.WriteString("\t")
			line.WriteString(string(p.StableID))
			line.WriteString("\t")
			line.WriteString(p.NodeKey.String())
			fmt.Println(line.String())
		}
	}

	return nil
}

var nlAddCmd = &ffcli.Command{
	Name:       "add",
	ShortUsage: "add <public-key>...",
	ShortHelp:  "Adds one or more trusted signing keys to miragenet lock",
	LongHelp:   "Adds one or more trusted signing keys to miragenet lock",
	Exec: func(ctx context.Context, args []string) error {
		return runNetworkLockModify(ctx, args, nil)
	},
}

var nlRemoveArgs struct {
	resign bool
}

var nlRemoveCmd = &ffcli.Command{
	Name:       "remove",
	ShortUsage: "remove [--re-sign=false] <public-key>...",
	ShortHelp:  "Removes one or more trusted signing keys from miragenet lock",
	LongHelp:   "Removes one or more trusted signing keys from miragenet lock",
	Exec:       runNetworkLockRemove,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("lock remove")
		fs.BoolVar(&nlRemoveArgs.resign, "re-sign", true, "resign signatures which would be invalidated by removal of trusted signing keys")
		return fs
	})(),
}

func runNetworkLockRemove(ctx context.Context, args []string) error {
	removeKeys, _, err := parseNLArgs(args, true, false)
	if err != nil {
		return err
	}
	st, err := localClient.NetworkLockStatus(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}
	if !st.Enabled {
		return errors.New("tailnet lock is not enabled")
	}

	if nlRemoveArgs.resign {
		// Validate we are not removing trust in ourselves while resigning. This is because
		// we resign with our own key, so the signatures would be immediately invalid.
		for _, k := range removeKeys {
			kID, err := k.ID()
			if err != nil {
				return fmt.Errorf("computing KeyID for key %v: %w", k, err)
			}
			if bytes.Equal(st.PublicKey.KeyID(), kID) {
				return errors.New("cannot remove local trusted signing key while resigning; run command on a different node or with --re-sign=false")
			}
		}

		// Resign affected signatures for each of the keys we are removing.
		for _, k := range removeKeys {
			kID, _ := k.ID() // err already checked above
			sigs, err := localClient.NetworkLockAffectedSigs(ctx, kID)
			if err != nil {
				return fmt.Errorf("affected sigs for key %X: %w", kID, err)
			}

			for _, sigBytes := range sigs {
				var sig tka.NodeKeySignature
				if err := sig.Unserialize(sigBytes); err != nil {
					return fmt.Errorf("failed decoding signature: %w", err)
				}
				var nodeKey key.NodePublic
				if err := nodeKey.UnmarshalBinary(sig.Pubkey); err != nil {
					return fmt.Errorf("failed decoding pubkey for signature: %w", err)
				}

				// Safety: NetworkLockAffectedSigs() verifies all signatures before
				// successfully returning.
				rotationKey, _ := sig.UnverifiedWrappingPublic()
				if err := localClient.NetworkLockSign(ctx, nodeKey, []byte(rotationKey)); err != nil {
					return fmt.Errorf("failed to sign %v: %w", nodeKey, err)
				}
			}
		}
	}

	return localClient.NetworkLockModify(ctx, nil, removeKeys)
}

// parseNLArgs parses a slice of strings into slices of tka.Key & disablement
// values/secrets.
// The keys encoded in args should be specified using their key.NLPublic.MarshalText
// representation with an optional '?<votes>' suffix.
// Disablement values or secrets must be encoded in hex with a prefix of 'disablement:' or
// 'disablement-secret:'.
//
// If any element could not be parsed,
// a nil slice is returned along with an appropriate error.
func parseNLArgs(args []string, parseKeys, parseDisablements bool) (keys []tka.Key, disablements [][]byte, err error) {
	for i, a := range args {
		if parseDisablements && (strings.HasPrefix(a, "disablement:") || strings.HasPrefix(a, "disablement-secret:")) {
			b, err := hex.DecodeString(a[strings.Index(a, ":")+1:])
			if err != nil {
				return nil, nil, fmt.Errorf("parsing disablement %d: %v", i+1, err)
			}
			disablements = append(disablements, b)
			continue
		}

		if !parseKeys {
			return nil, nil, fmt.Errorf("parsing argument %d: expected value with \"disablement:\" or \"disablement-secret:\" prefix, got %q", i+1, a)
		}

		var nlpk key.NLPublic
		spl := strings.SplitN(a, "?", 2)
		if err := nlpk.UnmarshalText([]byte(spl[0])); err != nil {
			return nil, nil, fmt.Errorf("parsing key %d: %v", i+1, err)
		}

		k := tka.Key{
			Kind:   tka.Key25519,
			Public: nlpk.Verifier(),
			Votes:  1,
		}
		if len(spl) > 1 {
			votes, err := strconv.Atoi(spl[1])
			if err != nil {
				return nil, nil, fmt.Errorf("parsing key %d votes: %v", i+1, err)
			}
			k.Votes = uint(votes)
		}
		keys = append(keys, k)
	}
	return keys, disablements, nil
}

func runNetworkLockModify(ctx context.Context, addArgs, removeArgs []string) error {
	st, err := localClient.NetworkLockStatus(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}
	if !st.Enabled {
		return errors.New("miragenet lock is not enabled")
	}

	addKeys, _, err := parseNLArgs(addArgs, true, false)
	if err != nil {
		return err
	}
	removeKeys, _, err := parseNLArgs(removeArgs, true, false)
	if err != nil {
		return err
	}

	if err := localClient.NetworkLockModify(ctx, addKeys, removeKeys); err != nil {
		return err
	}
	return nil
}

var nlSignCmd = &ffcli.Command{
	Name:       "sign",
	ShortUsage: "sign <node-key> [<rotation-key>]",
	ShortHelp:  "Signs a node key and transmits the signature to the coordination server",
	LongHelp:   "Signs a node key and transmits the signature to the coordination server",
	Exec:       runNetworkLockSign,
}

func runNetworkLockSign(ctx context.Context, args []string) error {
	var (
		nodeKey     key.NodePublic
		rotationKey key.NLPublic
	)

	if len(args) == 0 || len(args) > 2 {
		return errors.New("usage: lock sign <node-key> [<rotation-key>]")
	}
	if err := nodeKey.UnmarshalText([]byte(args[0])); err != nil {
		return fmt.Errorf("decoding node-key: %w", err)
	}
	if len(args) > 1 {
		if err := rotationKey.UnmarshalText([]byte(args[1])); err != nil {
			return fmt.Errorf("decoding rotation-key: %w", err)
		}
	}

	return localClient.NetworkLockSign(ctx, nodeKey, []byte(rotationKey.Verifier()))
}

var nlDisableCmd = &ffcli.Command{
	Name:       "disable",
	ShortUsage: "disable <disablement-secret>",
	ShortHelp:  "Consumes a disablement secret to shut down miragenet lock for the miragenet",
	LongHelp: strings.TrimSpace(`

The 'mirage lock disable' command uses the specified disablement
secret to disable miragenet lock.

If miragenet lock is re-enabled, new disablement secrets can be generated.

Once this secret is used, it has been distributed
to all nodes in the miragenet and should be considered public.

`),
	Exec: runNetworkLockDisable,
}

func runNetworkLockDisable(ctx context.Context, args []string) error {
	_, secrets, err := parseNLArgs(args, false, true)
	if err != nil {
		return err
	}
	if len(secrets) != 1 {
		return errors.New("usage: lock disable <disablement-secret>")
	}
	return localClient.NetworkLockDisable(ctx, secrets[0])
}

var nlLocalDisableCmd = &ffcli.Command{
	Name:       "local-disable",
	ShortUsage: "local-disable",
	ShortHelp:  "Disables miragenet lock for this node only",
	LongHelp: strings.TrimSpace(`

The 'mirage lock local-disable' command disables miragenet lock for only
the current node.

If the current node is locked out, this does not mean that it can initiate
connections in a miragenet with miragenet lock enabled. Rather, this means
that the current node will accept traffic from other nodes in the miragenet
that are locked out.

`),
	Exec: runNetworkLockLocalDisable,
}

func runNetworkLockLocalDisable(ctx context.Context, args []string) error {
	return localClient.NetworkLockForceLocalDisable(ctx)
}

var nlDisablementKDFCmd = &ffcli.Command{
	Name:       "disablement-kdf",
	ShortUsage: "disablement-kdf <hex-encoded-disablement-secret>",
	ShortHelp:  "Computes a disablement value from a disablement secret (advanced users only)",
	LongHelp:   "Computes a disablement value from a disablement secret (advanced users only)",
	Exec:       runNetworkLockDisablementKDF,
}

func runNetworkLockDisablementKDF(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lock disablement-kdf <hex-encoded-disablement-secret>")
	}
	secret, err := hex.DecodeString(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("disablement:%x\n", tka.DisablementKDF(secret))
	return nil
}

var nlLogArgs struct {
	limit int
	json  bool
}

var nlLogCmd = &ffcli.Command{
	Name:       "log",
	ShortUsage: "log [--limit N]",
	ShortHelp:  "List changes applied to miragenet lock",
	LongHelp:   "List changes applied to miragenet lock",
	Exec:       runNetworkLockLog,
	FlagSet: (func() *flag.FlagSet {
		fs := newFlagSet("lock log")
		fs.IntVar(&nlLogArgs.limit, "limit", 50, "max number of updates to list")
		fs.BoolVar(&nlLogArgs.json, "json", false, "output in JSON format (WARNING: format subject to change)")
		return fs
	})(),
}

func nlDescribeUpdate(update ipnstate.NetworkLockUpdate, color bool) (string, error) {
	terminalYellow := ""
	terminalClear := ""
	if color {
		terminalYellow = "\x1b[33m"
		terminalClear = "\x1b[0m"
	}

	var stanza strings.Builder
	printKey := func(key *tka.Key, prefix string) {
		fmt.Fprintf(&stanza, "%sType: %s\n", prefix, key.Kind.String())
		if keyID, err := key.ID(); err == nil {
			fmt.Fprintf(&stanza, "%sKeyID: %x\n", prefix, keyID)
		} else {
			// Older versions of the client shouldn't explode when they encounter an
			// unknown key type.
			fmt.Fprintf(&stanza, "%sKeyID: <Error: %v>\n", prefix, err)
		}
		if key.Meta != nil {
			fmt.Fprintf(&stanza, "%sMetadata: %+v\n", prefix, key.Meta)
		}
	}

	var aum tka.AUM
	if err := aum.Unserialize(update.Raw); err != nil {
		return "", fmt.Errorf("decoding: %w", err)
	}

	fmt.Fprintf(&stanza, "%supdate %x (%s)%s\n", terminalYellow, update.Hash, update.Change, terminalClear)

	switch update.Change {
	case tka.AUMAddKey.String():
		printKey(aum.Key, "")
	case tka.AUMRemoveKey.String():
		fmt.Fprintf(&stanza, "KeyID: %x\n", aum.KeyID)

	case tka.AUMUpdateKey.String():
		fmt.Fprintf(&stanza, "KeyID: %x\n", aum.KeyID)
		if aum.Votes != nil {
			fmt.Fprintf(&stanza, "Votes: %d\n", aum.Votes)
		}
		if aum.Meta != nil {
			fmt.Fprintf(&stanza, "Metadata: %+v\n", aum.Meta)
		}

	case tka.AUMCheckpoint.String():
		fmt.Fprintln(&stanza, "Disablement values:")
		for _, v := range aum.State.DisablementSecrets {
			fmt.Fprintf(&stanza, " - %x\n", v)
		}
		fmt.Fprintln(&stanza, "Keys:")
		for _, k := range aum.State.Keys {
			printKey(&k, "  ")
		}

	default:
		// Print a JSON encoding of the AUM as a fallback.
		e := json.NewEncoder(&stanza)
		e.SetIndent("", "\t")
		if err := e.Encode(aum); err != nil {
			return "", err
		}
		stanza.WriteRune('\n')
	}

	return stanza.String(), nil
}

func runNetworkLockLog(ctx context.Context, args []string) error {
	updates, err := localClient.NetworkLockLog(ctx, nlLogArgs.limit)
	if err != nil {
		return fixTailscaledConnectError(err)
	}
	if nlLogArgs.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(updates)
	}

	useColor := isatty.IsTerminal(os.Stdout.Fd())

	stdOut := colorable.NewColorableStdout()
	for _, update := range updates {
		stanza, err := nlDescribeUpdate(update, useColor)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdOut, stanza)
	}
	return nil
}

var nlTskeyWrapCmd = &ffcli.Command{
	Name:       "tskey-wrap",
	ShortUsage: "tskey-wrap <tailscale pre-auth key>",
	ShortHelp:  "Modifies a pre-auth key from the admin panel to work with tailnet lock",
	LongHelp:   "Modifies a pre-auth key from the admin panel to work with tailnet lock",
	Exec:       runTskeyWrapCmd,
}

func runTskeyWrapCmd(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lock tskey-wrap <tailscale pre-auth key>")
	}
	if strings.Contains(args[0], "--TL") {
		return errors.New("Error: provided key was already wrapped")
	}

	st, err := localClient.StatusWithoutPeers(ctx)
	if err != nil {
		return fixTailscaledConnectError(err)
	}

	// Generate a separate tailnet-lock key just for the credential signature.
	// We use the free-form meta strings to mark a little bit of metadata about this
	// key.
	priv := key.NewNLPrivate()
	m := map[string]string{
		"purpose":            "pre-auth key",
		"wrapper_stableid":   string(st.Self.ID),
		"wrapper_createtime": fmt.Sprint(time.Now().Unix()),
	}
	if strings.HasPrefix(args[0], "tskey-auth-") && strings.Index(args[0][len("tskey-auth-"):], "-") > 0 {
		// We don't want to accidentally embed the nonce part of the authkey in
		// the event the format changes. As such, we make sure its in the format we
		// expect (tskey-auth-<stableID, inc CNTRL suffix>-nonce) before we parse
		// out and embed the stableID.
		s := strings.TrimPrefix(args[0], "tskey-auth-")
		m["authkey_stableid"] = s[:strings.Index(s, "-")]
	}
	k := tka.Key{
		Kind:   tka.Key25519,
		Public: priv.Public().Verifier(),
		Votes:  1,
		Meta:   m,
	}

	wrapped, err := localClient.NetworkLockWrapPreauthKey(ctx, args[0], priv)
	if err != nil {
		return fmt.Errorf("wrapping failed: %w", err)
	}
	if err := localClient.NetworkLockModify(ctx, []tka.Key{k}, nil); err != nil {
		return fmt.Errorf("add key failed: %w", err)
	}

	fmt.Println(wrapped)
	return nil
}
