package pekit

import (
	"bytes"
	"io"
	"strings"
)

// Git release signatures are verified in-process like URL signatures: the
// mirror clone supplies the raw tag or commit object, pekit splits it into
// the signed payload and the embedded OpenPGP signature exactly as Git does,
// and the shared pinned-key policy decides. No host gpg, no keyring state,
// no gpg.program configuration. Verification runs after the mirror fetch,
// which only stores objects, and before the lock is written or anything is
// checked out.

// gitSignatureMarkers are the armor lines Git recognises as the start of an
// embedded signature. Only OpenPGP is verifiable; the others are named so a
// recipe pointed at an SSH- or X.509-signed upstream fails with a clear
// diagnosis rather than "unsigned".
var gitSignatureMarkers = []struct {
	prefix string
	format string
}{
	{"-----BEGIN PGP SIGNATURE-----", "openpgp"},
	{"-----BEGIN PGP MESSAGE-----", "openpgp"},
	{"-----BEGIN SSH SIGNATURE-----", "ssh"},
	{"-----BEGIN SIGNED MESSAGE-----", "x509"},
}

func gitSignatureFormat(line []byte) string {
	for _, marker := range gitSignatureMarkers {
		if bytes.HasPrefix(line, []byte(marker.prefix)) {
			return marker.format
		}
	}
	return ""
}

// verifyGitSignature verifies the signature cfg requires for ref, which
// resolved to commit in repo, and returns the signing key's primary
// fingerprint.
func verifyGitSignature(keyRoot string, cfg GitSignatureConfig, repo, ref, commit string) (string, error) {
	const field = "source.git.signature"
	var payload, sig []byte
	var what string
	var err error
	switch cfg.Object {
	case "commit":
		what = "commit " + commit + " signature"
		payload, sig, err = signedGitCommit(repo, commit)
	default:
		what = "tag " + ref + " signature"
		payload, sig, err = signedGitTag(repo, ref, commit)
	}
	if err != nil {
		return "", err
	}
	open := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	return verifyPinnedSignature(keyRoot, cfg.keyPolicy(), open, sig, what, field)
}

// signedGitTag reads the annotated tag ref names and splits it into the
// payload its signature covers and the signature itself. The tag must name
// ref and point directly at commit: a validly signed tag for some other
// release, or one that no longer points where ref resolved, proves nothing
// about this source.
func signedGitTag(repo, ref, commit string) ([]byte, []byte, error) {
	name := ref
	if strings.HasPrefix(name, "refs/") {
		if !strings.HasPrefix(name, "refs/tags/") {
			return nil, nil, diag("signature_missing", "source.git.ref %s is not a tag; sign-verify a branch with source.git.signature.object = \"commit\"", ref)
		}
		name = strings.TrimPrefix(name, "refs/tags/")
	}
	full := "refs/tags/" + name
	object, err := commandOutput(repo, "git", "rev-parse", "--verify", "--quiet", full)
	if err != nil {
		return nil, nil, diag("signature_missing", "source.git.ref %s is not a tag in the upstream repository, so it carries no tag signature", ref)
	}
	object = strings.TrimSpace(object)
	kind, err := commandOutput(repo, "git", "cat-file", "-t", object)
	if err != nil {
		return nil, nil, wrapDiag("git_resolve", "inspect tag "+name, err)
	}
	if strings.TrimSpace(kind) != "tag" {
		return nil, nil, diag("signature_missing", "tag %s is a lightweight tag and carries no signature — upstream may have stopped signing releases; verify why before removing [source.git.signature]", name)
	}
	raw, err := gitRawObject(repo, "tag", object)
	if err != nil {
		return nil, nil, wrapDiag("git_resolve", "read tag "+name, err)
	}
	headers := gitObjectHeaders(raw)
	if headers["tag"] != name {
		return nil, nil, diag("signature_invalid", "tag %s's object names itself %q — a signed tag of another release cannot vouch for this one", name, headers["tag"])
	}
	if headers["type"] != "commit" || headers["object"] != commit {
		return nil, nil, diag("signature_invalid", "tag %s points at %s %s, not the resolved commit %s", name, headers["type"], headers["object"], commit)
	}
	start, format := lastGitSignatureLine(raw)
	switch {
	case start < 0:
		return nil, nil, diag("signature_missing", "tag %s is annotated but unsigned — upstream may have stopped signing releases; verify why before removing [source.git.signature]", name)
	case format != "openpgp":
		return nil, nil, diag("signature_unsupported", "tag %s carries a %s signature; pekit verifies only OpenPGP", name, format)
	}
	return raw[:start], raw[start:], nil
}

// signedGitCommit reads commit and removes its gpgsig header, which is how
// Git reconstructs the payload a commit signature covers.
func signedGitCommit(repo, commit string) ([]byte, []byte, error) {
	raw, err := gitRawObject(repo, "commit", commit)
	if err != nil {
		return nil, nil, wrapDiag("git_resolve", "read commit "+commit, err)
	}
	headerEnd := bytes.Index(raw, []byte("\n\n"))
	if headerEnd < 0 {
		headerEnd = len(raw)
	}
	var payload, sig bytes.Buffer
	inSig, found := false, false
	for pos := 0; pos < len(raw); {
		end := bytes.IndexByte(raw[pos:], '\n')
		next := len(raw)
		if end >= 0 {
			next = pos + end + 1
		}
		line := raw[pos:next]
		if pos > headerEnd {
			payload.Write(raw[pos:])
			break
		}
		switch {
		case inSig && bytes.HasPrefix(line, []byte(" ")):
			sig.Write(line[1:])
		case bytes.HasPrefix(line, []byte("gpgsig-sha256 ")):
			return nil, nil, diag("signature_unsupported", "commit %s carries a SHA-256 object signature; pekit verifies SHA-1 repositories' gpgsig only", commit)
		case bytes.HasPrefix(line, []byte("gpgsig ")):
			if found {
				return nil, nil, diag("signature_invalid", "commit %s has more than one gpgsig header", commit)
			}
			inSig, found = true, true
			sig.Write(line[len("gpgsig "):])
		default:
			inSig = false
			payload.Write(line)
		}
		pos = next
	}
	if !found {
		return nil, nil, diag("signature_missing", "commit %s is unsigned — upstream may have stopped signing releases; verify why before removing [source.git.signature]", commit)
	}
	if format := gitSignatureFormat(sig.Bytes()); format != "openpgp" {
		if format == "" {
			format = "unrecognised"
		}
		return nil, nil, diag("signature_unsupported", "commit %s carries a %s signature; pekit verifies only OpenPGP", commit, format)
	}
	return payload.Bytes(), sig.Bytes(), nil
}

// gitRawObject returns an object's exact bytes. commandOutput merges stderr
// and would corrupt the payload a signature covers.
func gitRawObject(repo, kind, object string) ([]byte, error) {
	cmd := execCommand("git", "cat-file", kind, object)
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, diag("git_resolve", "git cat-file %s %s: %v: %s", kind, object, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitObjectHeaders returns the single-valued headers ahead of an object's
// first blank line. Continuation lines belong to multi-line headers pekit
// does not read here.
func gitObjectHeaders(raw []byte) map[string]string {
	headers := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, " ") {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		if _, seen := headers[key]; !seen {
			headers[key] = value
		}
	}
	return headers
}

// lastGitSignatureLine mirrors Git's parse_signed_buffer: the embedded
// signature starts at the last line beginning with a signature marker, and
// everything before it is the signed payload.
func lastGitSignatureLine(raw []byte) (int, string) {
	start, format := -1, ""
	for pos := 0; pos < len(raw); {
		if f := gitSignatureFormat(raw[pos:]); f != "" {
			start, format = pos, f
		}
		end := bytes.IndexByte(raw[pos:], '\n')
		if end < 0 {
			break
		}
		pos += end + 1
	}
	return start, format
}
