// Command releasekey makes and uses the starfix release signing key. It is
// not part of sfx or starfixd.
//
//	releasekey gen KEYFILE            new key pair: private key to KEYFILE (0600), public key to stdout
//	releasekey sign FILE              sign FILE into FILE.sig with $STARFIX_RELEASE_KEY
//	releasekey verify FILE [SIG]      check SIG (default FILE.sig) against the keys in keys.go
//
// The private key is the standard base64 of the 32-byte Ed25519 seed. It
// never appears in argv or output: gen writes it only to KEYFILE, and sign
// reads it only from the environment. See RELEASING.md.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/release"
)

// EnvKey holds the private key for sign.
const EnvKey = "STARFIX_RELEASE_KEY"

const usage = `usage:
  releasekey gen KEYFILE
  releasekey sign FILE                (private key in $STARFIX_RELEASE_KEY)
  releasekey verify [--key BASE64]... FILE [SIG]`

type env struct {
	getenv         func(string) string
	stdout, stderr io.Writer
	// keys returns the trusted public keys. Default release.Keys.
	keys func() ([]ed25519.PublicKey, error)
}

type usageError string

func (e usageError) Error() string { return string(e) }

func main() {
	os.Exit(run(os.Args[1:], env{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr, keys: release.Keys}))
}

func run(args []string, e env) int {
	var err error
	switch {
	case len(args) == 0:
		err = usageError("no command")
	case args[0] == "gen":
		err = gen(args[1:], e)
	case args[0] == "sign":
		err = sign(args[1:], e)
	case args[0] == "verify":
		err = verify(args[1:], e)
	default:
		err = usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintf(e.stderr, "releasekey: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) {
		_, _ = fmt.Fprintln(e.stderr, usage)
		return 2
	}
	return 1
}

// gen writes a new private key to KEYFILE, refusing to overwrite one, and
// prints the public key as the line to add to keys.go.
func gen(args []string, e env) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageError("gen takes one KEYFILE")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the maintainer names the key file
	if err != nil {
		return fmt.Errorf("%w; fix: name a file that does not exist yet", err)
	}
	_, werr := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed()))
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		_ = os.Remove(args[0]) //nolint:gosec // the file gen just created
		return fmt.Errorf("write %s: %w", args[0], err)
	}
	_, _ = fmt.Fprintf(e.stdout, "\t%q,\n", release.EncodeKey(pub))
	_, _ = fmt.Fprintf(e.stderr, "private key written to %s (mode 0600); add the line above to internal/release/keys.go\n", args[0])
	return nil
}

// sign writes FILE.sig, after checking the key is one keys.go trusts, so a
// release is never published with a signature clients would refuse.
func sign(args []string, e env) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageError("sign takes one FILE")
	}
	priv, err := privateKey(e.getenv(EnvKey))
	if err != nil {
		return err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return errors.New("derive public key")
	}
	trusted, err := e.keys()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(trusted, func(k ed25519.PublicKey) bool { return pub.Equal(k) }) {
		return fmt.Errorf("the key in $%s (public %s) is not in internal/release/keys.go; fix: add it there by pull request before releasing, or set the right secret", EnvKey, release.EncodeKey(pub))
	}
	msg, err := os.ReadFile(args[0]) //nolint:gosec // the workflow names the file to sign
	if err != nil {
		return fmt.Errorf("%w; fix: name the checksums file to sign", err)
	}
	sig := release.EncodeSignature(ed25519.Sign(priv, msg))
	if err := (release.Ed25519{Keys: []ed25519.PublicKey{pub}}).Verify(context.Background(), msg, sig, ""); err != nil {
		return fmt.Errorf("self-check: %w", err)
	}
	out := args[0] + ".sig"
	tmp, err := os.CreateTemp(filepath.Dir(out), ".sig-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(sig)
	if err := errors.Join(werr, tmp.Close(), os.Chmod(tmp.Name(), 0o644), os.Rename(tmp.Name(), out)); err != nil { //nolint:gosec // a signature is public
		_ = os.Remove(tmp.Name()) //nolint:gosec // the temporary file sign just made
		return fmt.Errorf("write %s: %w", out, err)
	}
	_, _ = fmt.Fprintf(e.stderr, "signed %s with %s\n", args[0], release.EncodeKey(pub))
	return nil
}

func privateKey(v string) (ed25519.PrivateKey, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, fmt.Errorf("$%s is empty; fix: run in the release environment, whose secret holds the key", EnvKey)
	}
	b, err := base64.StdEncoding.DecodeString(v)
	switch {
	case err != nil:
		// The decoder's error can quote input; never echo the secret.
		return nil, fmt.Errorf("$%s is not base64; fix: set it to the contents of the file `releasekey gen` wrote", EnvKey)
	case len(b) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, fmt.Errorf("$%s holds %d bytes, not a 32-byte Ed25519 seed; fix: set it to the contents of the file `releasekey gen` wrote", EnvKey, len(b))
}

type keyList []string

func (k *keyList) String() string     { return strings.Join(*k, ",") }
func (k *keyList) Set(v string) error { *k = append(*k, v); return nil }

// verify checks SIG over FILE against keys.go's keys, or the --key ones.
func verify(args []string, e env) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var given keyList
	fs.Var(&given, "key", "trust this base64 public key instead of keys.go (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return usageError("verify takes FILE and an optional SIG")
	}
	file, sigFile := fs.Arg(0), fs.Arg(0)+".sig"
	if fs.NArg() == 2 {
		sigFile = fs.Arg(1)
	}
	keys, err := e.keys()
	if len(given) > 0 {
		keys, err = release.ParseKeys(given)
	}
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("%w; fix: pass --key with a key from internal/release/keys.go at the release's tag", release.ErrNoKey)
	}
	msg, err := os.ReadFile(file) //nolint:gosec // the person names the file
	if err != nil {
		return fmt.Errorf("%w; fix: download checksums.txt from the release", err)
	}
	sig, err := os.ReadFile(sigFile) //nolint:gosec // the person names the file
	if err != nil {
		return fmt.Errorf("%w; fix: download checksums.txt.sig from the release", err)
	}
	if err := (release.Ed25519{Keys: keys}).Verify(context.Background(), msg, sig, ""); err != nil {
		return fmt.Errorf("%s: %w; fix: do not use these files", file, err)
	}
	_, _ = fmt.Fprintf(e.stdout, "%s: good signature\n", file)
	return nil
}
