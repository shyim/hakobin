# Code Review: `hakobin`

**Date:** 2026-07-05
**Scope:** Full codebase (~4,400 lines Go) — a CLI that builds and hosts APT/DEB and RPM/YUM
repositories on S3-compatible storage with GPG signing and CloudFront/Cloudflare CDN invalidation.
**Method:** Toolchain baseline + four focused reviews (crypto/signing, DEB/APT, RPM, storage/state);
every critical/high finding verified against source.

---

## Baseline (all green)

| Check | Result |
|-------|--------|
| `go build ./...` | ✓ |
| `go vet ./...` | ✓ |
| `golangci-lint run` | 0 issues |
| unit tests (apt, deb, openpgp, repository, rpm) | all pass |
| e2e (Testcontainers/MinIO) | not run — requires Docker |

The code is clean, idiomatic, and well-tested at the unit level. The problems are **behavioral** —
things unit tests don't exercise: security of untrusted inputs, concurrency, and silent failure modes.

---

## 🔴 Critical / High — fix before real use

> **Status:** All 8 items below were fixed on branch `fix/high-severity-review`.
> Verified with `go build`, `go vet`, `golangci-lint` (0 issues), the unit suite,
> and the Testcontainers/MinIO e2e suite (both apt and rpm workflows pass).
> Each fix has accompanying unit tests. The 🟡 Medium / 🟢 Low items below are
> not yet addressed (except `isNotFoundError`, hardened as a side effect of #5).


### 1. Requested-but-unloadable signing key silently produces an *unsigned* repo, exit 0
`internal/openpgp/loader.go:19-20, 39-40`

A typo in `--signing-key`, a missing file, or malformed `GPG_PRIVATE_KEY` only prints `Warning: …`
and continues with `active == nil`. `updateRelease` / RPM `writeMetadata` then skip signing entirely
and upload unsigned `Release`/`repomd.xml` while reporting success. In CI this ships an unsigned repo
that looks fine.

**Fix:** If a signing key was *requested* (env set or `--signing-key` passed) but failed to load,
return a hard error instead of a warning.

### 2. Path traversal from package- and CLI-controlled fields into S3 keys
`internal/repository/repository.go:937` (`key()` does no sanitization), `internal/deb/deb.go:94-108`,
`internal/rpm/repository.go:346,486`

`key()` just prefixes and trims a leading `/`. A malicious `.deb` with `Package`/`Version`/`Architecture`
containing `../`, or RPM CLI flags like `--repo ../../deb`, produce keys with `..` segments passed
straight to `UploadBytes`. On backends that resolve `..` (MinIO/filesystem gateways, local caches)
this overwrites other repos' `repomd.xml`, `Release`, `apt-repo.json`, or pubkeys.

**Fix:** Validate `Package`/`Version`/`Architecture` against the Debian charset `[A-Za-z0-9][A-Za-z0-9+.-]*`;
reject `repo`/`arch` containing path separators or `.`/`..`.

### 3. Metadata injection via newlines in DEB control fields
`internal/deb/deb.go:110-127`

`PackageEntry` emits every control value verbatim with `%s: %s\n`. An embedded `\n` lets a crafted
`.deb` inject fake stanzas or a `Filename:` line pointing apt at an arbitrary object.

**Fix:** Reject/escape control values containing newlines before writing metadata.

### 4. `Architecture: all` packages are uploaded but invisible to apt
`internal/repository/repository.go:636` (`ensureArchitecture` early-returns on `all`),
`internal/repository/repository.go:679-682` (`updateRelease` filters out `all`)

Arch-`all` `.deb`s get written to `binary-all/Packages`, but `all` is never added to metadata and
never referenced in the `Release` file. Clients configured for `amd64` never see them.

**Fix:** Merge `all` packages into every concrete `binary-<arch>/Packages` index (standard Debian behavior).

### 5. Non-atomic read-modify-write on `Packages` and `apt-repo.json` → lost packages under concurrency
`internal/repository/repository.go:545-571`, `635-674`

Upload does `Download → string-concat → Upload` with no lock, ETag, or conditional write. Two parallel
`hakobin deb upload` runs (the obvious CI trigger) clobber each other: a `.deb` lands in the pool but
is dropped from `Packages`. Same race loses architectures in `apt-repo.json`.

**Fix:** Use S3 conditional writes (`If-Match` on ETag, retry on 412) or a lock object. At minimum,
document that concurrent uploads are unsafe.

### 6. `remove` deletes the pool blob *before* reindexing, and swallows the reindex errors
`internal/repository/repository.go:404-436`

Delete happens first (line 404), then `Packages` rewrite with `_ =` ignored errors (424, 428). If the
rewrite fails, the blob is gone but `Packages`/`Release` still advertise it → apt 404s — while the user
is told "removed successfully."

**Fix:** Reindex first, delete last; propagate the upload errors.

### 7. Extra `--signing-key` private files silently dropped from the trusted bundle when `GPG_PRIVATE_KEY` is set
`internal/openpgp/loader.go:52` → `LoadPublicKeyCerts` (only parses *public* blocks)

Rotation via env-active + multiple private `--signing-key` files silently loses the old keys → clients
reject packages signed by the rotated-out key during the overlap window.

**Fix:** Accept private-key blocks in the trusted-key loader (extract the public cert), or document that
rotation keys must use `--trusted-key`.

### 8. Encrypted (passphrase-protected) private keys are never checked or decrypted
`internal/openpgp/openpgp.go:138-150`, used at `internal/rpm/rpm.go:222`

No `PrivateKey.Encrypted` check → DEB signing fails at sign time; RPM signing passes an encrypted key
to `SignRpmFile` producing an error or bad signature.

**Fix:** Detect `Encrypted` and either reject clearly or decrypt via a `GPG_PASSPHRASE` env var.

---

## 🟡 Medium

> **Status:** All 11 medium items below were fixed on branch `fix/high-severity-review`.
> Verified with `go build`, `go vet`, `golangci-lint` (0 issues), the unit suite
> (with added tests), and the MinIO e2e suite. The 🟢 Low items remain open.


- **No `InRelease` (clearsigned) file** — only detached `Release.gpg`. Modern apt prefers `InRelease`.
  `internal/repository/repository.go:728-745`
- **CDN invalidation misses overwritten pool blobs.** On `--force` re-upload and on remove, the `.deb`
  key is re-written but never invalidated → CloudFront serves old bytes while `Packages` advertises the
  new hash → apt hash-mismatch until TTL. `internal/repository/repository.go:753-780`
- **CloudFront uses a *separate* default AWS credential/region chain** (`internal/cdn/cdn.go:80`,
  `LoadDefaultConfig`) instead of the configured S3 creds — inconsistent, breaks with MinIO-style setups.
- **CDN `FromEnv` errors are silently ignored** (`internal/repository/repository.go:754`:
  `err == nil && invalidator != nil`) — a typo'd `HAKOBIN_CDN_PURGE_TYPE` disables purging with no signal.
- **Whole package buffered in memory** and uploaded via single `PutObject` (5 GiB cap, OOM risk); no
  multipart/streaming. `internal/storage/storage.go:65-76`, `internal/deb/deb.go:30`
- **`isNotFoundError` uses string/`%T` matching instead of `errors.As`** — fragile against SDK error
  wrapping. `internal/storage/storage.go:150-168`
- **No `Cache-Control` on metadata objects** — increases reliance on invalidation for
  `Release`/`Packages` freshness. `internal/storage/storage.go:65-71`
- **RPM `<time file>` uses `time.Now()`** on every regeneration (non-deterministic, breaks mirror
  caching); **`<rpm:header-range>` hard-coded to `0/0`**. `internal/rpm/rpm.go:496, 505`
- **RPM decompression bombs**: control-tar and rpm streams `io.ReadAll` with no `LimitReader` cap.
  `internal/deb/deb.go:179-206`
- **RPM `--arch` vs `--repo-arch` trap**: removing a `noarch` package requires `--arch noarch`; the
  natural `--arch x86_64` silently reports "not found." `internal/rpm/repository.go:219`
- **Batch multi-arch upload** can leave earlier distributions' `Release` missing later-added
  architectures (metadata loaded once, `Release` written per-file). `internal/repository/repository.go:185`
- **`loadMetadata` error silently downgraded to "no metadata"** → transient S3 error rewrites `Release`
  with degraded/empty metadata. `internal/repository/repository.go:185-189`

---

## 🟢 Low (worth a sweep)

> **Status:** All fixed on branch `fix/high-severity-review`. Note the last item
> uncovered a real bug: multi-key public bundles only parsed back the first key,
> which would have silently broken key rotation on clients — now fixed and tested.

- ✅ Stale `repomd.xml.asc`/pubkey deleted when a repo goes from signed→unsigned.
- ✅ Orphaned checksum-named repodata garbage-collected against the new `repomd.xml`.
- ✅ `truncate` no longer panics for `max<3` (fixed with the high-severity batch).
- ✅ `S3_USE_PATH_STYLE` parsed with the common truthy spellings (`parseBool`).
- ✅ `HAKOBIN_PUBLIC_URL` validated as a well-formed http(s) URL in `RequireS3`.
- ✅ `RepositoryBaseURL` emits a path-style URL when `S3UsePathStyle` is set for AWS.
- ✅ Multi-key armor bundle is now built deterministically **and round-trip
  verified**; the verification exposed and fixed a parser bug where only the
  first armored block was read.

---

## What's correct (verified, not assumed)

- SHA-256 throughout signing — **no weak signing hashes**, no key material logged or written to disk
  during signing.
- DEB Release hash-section format and per-variant (compressed/uncompressed) size+hash are right.
- RPM repomd open-vs-compressed checksums/sizes, package counts, and href suffixes match `createrepo`
  semantics; the detached `.asc` signs exactly the stored `repomd.xml` bytes; package checksums are over
  the *signed* artifact.
- S3 path-style/virtual-host wiring and `ListKeys` pagination are correct.

---

## Bottom line

Solid, well-structured code with good unit coverage, but it currently assumes a **single trusted
uploader and well-formed packages**. The must-fix cluster is:

1. Silent-unsigned on key-load failure (#1)
2. Input validation on package fields and CLI paths (#2, #3)
3. Arch-`all` handling (#4)
4. Atomicity + remove-ordering (#5, #6)

Those turn "looks like it worked" into real correctness/security guarantees.

**Suggested order:** start with #1 and #6 (small, high-impact, easy to unit-test), then the
input-validation cluster (#2/#3), then arch-`all` (#4), then the concurrency work (#5).
