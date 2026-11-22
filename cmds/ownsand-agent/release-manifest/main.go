// SPDX-License-Identifier: FSL-1.1-ALv2

// Command release-manifest maintains the signed release manifest of the
// OwnSand device agent. The release workflow (.github/workflows/deploy.yml,
// through scripts/ci/release-binaries.sh) runs it; maintainers run it to
// create the release key pair. It imports only the standard library and
// repository packages that do, because the `sign` subcommand runs next to
// the release signing key.
//
// Usage:
//
//	release-manifest keygen <private-key-file>
//	release-manifest public-key < <private-key-file>
//	release-manifest check-keys
//	release-manifest add-release [--current <manifest> --current-signature <sig>]
//	    --version <version> --binary <name> --platforms <list>
//	    --artifacts <dir> --url-prefix <url> --out <manifest>
//	release-manifest sign --manifest <manifest> --out <sig>
//	release-manifest verify --manifest <manifest> --signature <sig>
//
// `sign` reads the PKCS#8 PEM private key from the environment variable
// OWNSAND_AGENT_RELEASE_SIGNING_KEY. Exit codes: 0 success, 1 error, 2 usage
// error.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/releasekeys"
)

// EnvSigningKey holds the PKCS#8 PEM Ed25519 release private key in the
// release signing step.
const EnvSigningKey = "OWNSAND_AGENT_RELEASE_SIGNING_KEY"

// builtinKeys returns the release public keys built into the agent. Tests
// replace it.
var builtinKeys = releasekeys.PublicKeys

type usageError struct{ error }

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := run(os.Args[1:], os.Stdin, os.Stdout, os.Getenv, logger)
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		fmt.Fprintln(os.Stderr, "release-manifest:", err)
		os.Exit(2)
	default:
		logger.Error("release manifest command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer, getenv func(string) string, logger *slog.Logger) error {
	if len(args) == 0 {
		return usageError{errors.New("missing subcommand: keygen, public-key, check-keys, add-release, sign or verify")}
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, logger)
	case "public-key":
		return runPublicKey(args[1:], stdin, stdout)
	case "check-keys":
		return runCheckKeys(args[1:], logger)
	case "add-release":
		return runAddRelease(args[1:], logger)
	case "sign":
		return runSign(args[1:], getenv, logger)
	case "verify":
		return runVerify(args[1:], logger)
	}
	return usageError{fmt.Errorf("unknown subcommand %q", args[0])}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string, required ...string) error {
	if err := fs.Parse(args); err != nil {
		return usageError{fmt.Errorf("%s: %w", fs.Name(), err)}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("%s: unexpected argument %q", fs.Name(), fs.Arg(0))}
	}
	for _, name := range required {
		if fs.Lookup(name).Value.String() == "" {
			return usageError{fmt.Errorf("%s: --%s is required", fs.Name(), name)}
		}
	}
	return nil
}

func loadBuiltinKeys() ([]ed25519.PublicKey, error) {
	keys, err := builtinKeys()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errNoBuiltinKeys
	}
	return keys, nil
}

// runKeygen writes a new PKCS#8 PEM private key to a new file with mode 0600
// and prints the base64 public key for package releasekeys.
func runKeygen(args []string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return usageError{errors.New("keygen: give the path of the new private key file")}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(args[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	logger.Info("release key pair generated", "private_key_file", args[0])
	_, err = fmt.Fprintln(stdout, releasekeys.Encode(pub))
	return err
}

func parsePrivateKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("the release private key is not a PKCS#8 PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the release private key is not a PKCS#8 private key")
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the release private key is not an Ed25519 key")
	}
	return priv, nil
}

func runPublicKey(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 0 {
		return usageError{errors.New("public-key: reads the private key from standard input and takes no arguments")}
	}
	pemBytes, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return err
	}
	priv, err := parsePrivateKey(pemBytes)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, releasekeys.Encode(priv.Public().(ed25519.PublicKey)))
	return err
}

func runCheckKeys(args []string, logger *slog.Logger) error {
	if len(args) != 0 {
		return usageError{errors.New("check-keys takes no arguments")}
	}
	keys, err := loadBuiltinKeys()
	if err != nil {
		return err
	}
	logger.Info("built-in release keys", "count", len(keys))
	return nil
}

func runAddRelease(args []string, logger *slog.Logger) error {
	fs := newFlagSet("add-release")
	current := fs.String("current", "", "current manifest (omit for the first release)")
	currentSig := fs.String("current-signature", "", "signature of the current manifest")
	version := fs.String("version", "", "release version")
	binary := fs.String("binary", "", "executable name")
	platformList := fs.String("platforms", "", "comma-separated <GOOS>-<GOARCH> list")
	artifacts := fs.String("artifacts", "", "directory with the executables")
	urlPrefix := fs.String("url-prefix", "", "download URL of the release assets")
	out := fs.String("out", "", "new manifest")
	if err := parseFlags(fs, args, "version", "binary", "platforms", "artifacts", "url-prefix", "out"); err != nil {
		return err
	}
	if (*current == "") != (*currentSig == "") {
		return usageError{errors.New("add-release: give --current and --current-signature together")}
	}
	var ps []Platform
	for _, s := range strings.Split(*platformList, ",") {
		p, err := parsePlatform(strings.TrimSpace(s))
		if err != nil {
			return usageError{fmt.Errorf("add-release: %w", err)}
		}
		ps = append(ps, p)
	}
	keys, err := loadBuiltinKeys()
	if err != nil {
		return err
	}
	m := emptyManifest()
	if *current != "" {
		manifestBytes, err := os.ReadFile(*current)
		if err != nil {
			return err
		}
		sigText, err := os.ReadFile(*currentSig)
		if err != nil {
			return err
		}
		// Never extend a manifest that the agents would reject: a tampered
		// object in the bucket must not get a valid signature.
		if err := verifySignature(manifestBytes, sigText, keys); err != nil {
			return err
		}
		if m, err = decodeManifest(manifestBytes); err != nil {
			return err
		}
	}
	r, err := newRelease(releaseInput{
		Version:     *version,
		Binary:      *binary,
		Platforms:   ps,
		ArtifactDir: *artifacts,
		URLPrefix:   *urlPrefix,
	})
	if err != nil {
		return err
	}
	if err := addRelease(m, r); err != nil {
		return err
	}
	b, err := m.encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	logger.Info("release added to the manifest", "version", r.Version, "platforms", len(r.Artifacts), "releases", len(m.Releases), "revoked_versions", len(m.RevokedVersions))
	return nil
}

func runSign(args []string, getenv func(string) string, logger *slog.Logger) error {
	fs := newFlagSet("sign")
	manifestPath := fs.String("manifest", "", "manifest to sign")
	out := fs.String("out", "", "signature file")
	if err := parseFlags(fs, args, "manifest", "out"); err != nil {
		return err
	}
	pemText := getenv(EnvSigningKey)
	if pemText == "" {
		return fmt.Errorf("%s is not set", EnvSigningKey)
	}
	priv, err := parsePrivateKey([]byte(pemText))
	if err != nil {
		return err
	}
	keys, err := loadBuiltinKeys()
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	if _, err := decodeManifest(manifestBytes); err != nil {
		return err
	}
	if len(manifestBytes) > maxManifestBytes {
		return errManifestTooLong
	}
	sigText, err := sign(manifestBytes, priv, keys)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, sigText, 0o644); err != nil {
		return err
	}
	logger.Info("release manifest signed", "manifest", *manifestPath, "signature", *out)
	return nil
}

func runVerify(args []string, logger *slog.Logger) error {
	fs := newFlagSet("verify")
	manifestPath := fs.String("manifest", "", "manifest")
	sigPath := fs.String("signature", "", "signature file")
	if err := parseFlags(fs, args, "manifest", "signature"); err != nil {
		return err
	}
	keys, err := loadBuiltinKeys()
	if err != nil {
		return err
	}
	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	sigText, err := os.ReadFile(*sigPath)
	if err != nil {
		return err
	}
	if err := verifySignature(manifestBytes, sigText, keys); err != nil {
		return err
	}
	m, err := decodeManifest(manifestBytes)
	if err != nil {
		return err
	}
	logger.Info("release manifest verified", "releases", len(m.Releases), "revoked_versions", len(m.RevokedVersions))
	return nil
}
