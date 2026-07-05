# Security & Documentation-Gap Review

Reviewed: 2026-07-03, full read of `kluster-lib` and `kluster` at commit `22fd495` plus the
in-flight signet-profile work (`kluster-lib/addon/signet.go`, `kluster-lib/profile/signet.go`,
modified `authstar.go`/`up.go`/`README.md`/`CLAUDE.md`).

**Instructions for the implementing model:** Work through items top to bottom (they are ordered
by severity within each section). For each item: make the fix, run the verification step, then
change `- [ ]` to `- [x]` and append a one-line note of what you did (and the commit, if you
commit per item). If you disagree with a finding or decide to accept the risk instead of fixing
it, do NOT silently skip it — check it off with a note starting `WONTFIX:` explaining why, so
every item ends the process explicitly resolved. Threat-model context: kluster is a **local
dev/CI tool**; in-cluster hardening findings are graded accordingly (deliberately-weak dev
credentials are acceptable *if documented and not exposed beyond the machine*), but anything
that touches the **host machine** (files, PATH, sudo, shared temp dirs, listening sockets) is
graded as real attack surface.

---

## 1. Security — High

- [x] **S1. `kluster setup` kubectl install: fixed `/tmp` path + `sudo mv` is a local privilege escalation (TOCTOU), and the download is not checksum-verified.**
  `kluster/cmd/setup.go:188-194` — `installKubectl` runs a shell script that downloads to the
  **fixed, predictable path** `/tmp/kubectl`, `chmod +x`s it, then `sudo mv`s it to
  `/usr/local/bin/kubectl`. On any multi-user host, another local user can pre-create or swap
  `/tmp/kubectl` (directly or via symlink) between the download and the `sudo mv`, getting an
  attacker-controlled binary installed root-blessed into `PATH`. Separately, the kubectl binary
  is downloaded with no integrity check even though `dl.k8s.io` publishes a `.sha256` beside
  every binary.
  **Fix:** download into a private `mktemp -d` directory (mode 0700), fetch
  `https://dl.k8s.io/release/${VERSION}/bin/linux/${ARCH}/kubectl.sha256` and verify with
  `sha256sum -c` (fail hard on mismatch), and install with
  `sudo install -o root -g root -m 0755 "$TMPDIR/kubectl" /usr/local/bin/kubectl` instead of
  `mv`. Prefer doing the download/verify in Go (the rest of kluster avoids shelling out) and
  only shell for the final `sudo install`.
  **Verify:** `go build ./...`; run `kluster setup --dry-run`; on a machine without kubectl (or
  by temporarily renaming it and pointing the installer at a scratch `INSTALL_DIR`), run the
  install path and confirm checksum verification executes and a corrupted download is rejected.

  **Resolution:** Fixed. kubectl now downloads to a private `mktemp -d` dir, verifies against `kubectl.sha256` (`sha256sum -c`, aborts on mismatch), and installs via `sudo install -o root -g root -m 0755`. `kluster/cmd/setup.go`.
- [x] **S2. Helm repository config and chart cache live in world-shared temp with 0755 perms — cross-user chart poisoning.**
  `kluster-lib/addon/addon.go:61-65` — `filepath.Join(os.TempDir(), "kluster", "helm-cache")`
  and `.../helm-repos.yaml`. `/tmp` is world-writable: whichever user creates `/tmp/kluster`
  first owns it, so on a shared machine another user can pre-create it and plant a poisoned
  chart cache or a `helm-repos.yaml` pointing at attacker-controlled repo URLs. Every chart
  kluster installs (SPIRE, cert-manager, ArgoCD…) runs privileged-ish workloads in the cluster,
  so this is arbitrary-workload injection. It also silently persists stale caches across runs.
  **Fix:** use `os.UserCacheDir()` → `<cache>/kluster/helm-cache` and
  `os.UserConfigDir()`/`<cache>/kluster/helm-repos.yaml`, created with `0o700`. Refuse to
  proceed (or recreate) if the directory exists but is not owned by the current user with mode
  0700.
  **Verify:** `go test ./...`; run `kluster up` and confirm the cache appears under
  `~/Library/Caches/kluster` (macOS) / `~/.cache/kluster` (Linux) with 0700, and nothing is
  created under `/tmp`.

  **Resolution:** Fixed. Helm cache/repo config moved to `os.UserCacheDir()`/`os.UserConfigDir()` (0700), off world-writable `/tmp`. `kluster-lib/addon/addon.go`.
- [x] **S3. Documented `--addon observability` / `--addon tracing` usage is broken: they are profiles, not addons.**
  (Filed under security because the *published* README teaches a command that hard-fails, and
  the failure message `unknown addon "observability"` will push users to improvise.)
  `kluster-lib/profile/observability.go:16` and `profile/tracing.go:16` register
  `observability`/`tracing` as **profiles** whose addons are `prometheus`/`grafana` and
  `loki`/`tempo`. But `kluster/cmd/up.go:24` help text, `README.md:77`, `README.md:200-211`,
  and `CLAUDE.md`'s CLI surface all pass them via `--addon`. `Cluster.collectAddons`
  (`kluster-lib/cluster/cluster.go:189-211`) feeds `--addon` values straight into the addon
  registry, so `kluster up --profile authstar --addon observability` fails in `topoSort` with
  `unknown addon "observability"` after the cluster has already been created (leaving a
  half-built cluster, see S15).
  **Fix (pick one, update all docs to match):** (a) make `--addon` values resolve against the
  profile registry too — if the name is a profile, expand it into the profile list (probably the
  intent: “optional addon groups”); or (b) make `--profile` repeatable and document
  `--profile authstar --profile observability`. Also add `argocd` to the `--addon` help text —
  it is a real addon and currently undocumented in the flag help.
  **Verify:** add a unit test in `kluster-lib/cluster/cluster_test.go` covering the chosen
  resolution; run `kluster up --profile spire --addon observability --name t1` end-to-end (or
  at minimum assert resolution order in the test) and confirm the README examples work as
  written.

  **Resolution:** Fixed. `collectAddons()` resolves an `--addon` value against the addon registry first, only expanding it as a profile's addon-group (observability/tracing) if no same-named addon exists — this also fixes a real bug the naive version of this fix introduced: "spire" and "signet" each name both an addon and their owning profile, so profile-first resolution would silently drop the addon. Regression tests added (`TestCollectAddons_ExtraProfileNameExpandsToItsAddons`, `TestCollectAddons_ExtraAddonNameSharedWithProfileNamePrefersAddon`). Docs updated (README, `up.go` help) to describe addon groups and mention `argocd`.
## 2. Security — Medium

- [x] **S4. k3d API server listens on `0.0.0.0` — the cluster API is reachable from the local network.**
  `kluster-lib/provider/k3d.go:45-56` builds a `SimpleConfig` with no `ExposeAPI`, so k3d
  defaults to binding the API-server load-balancer port on all interfaces
  (`k3d.DefaultAPIHost` = `0.0.0.0`, confirmed by the fallback at `k3d.go:183-185`). TLS +
  client-cert auth still gate access, but a dev cluster full of known-password services
  (Grafana `admin/admin`, RabbitMQ `guest/guest`, Dex, ArgoCD — see S7) should not be
  network-reachable at all on a coffee-shop LAN, and any future k8s API auth CVE becomes
  remotely exploitable.
  **Fix:** set `ExposeAPI.HostIP = "127.0.0.1"` in the SimpleConfig (k3d supports this
  directly), and in `fixKubeconfigServerPort` map an empty/`0.0.0.0` host IP to `127.0.0.1`
  when writing the kubeconfig server URL. Consider an opt-out flag (`--api-host`) for users who
  genuinely need LAN access. Check whether kind exposes the same issue
  (`kind` binds to 127.0.0.1 by default — confirm and note in code comment).
  **Verify:** `kluster up --profile spire --name t1`, then `docker port k3d-t1-serverlb` shows
  `127.0.0.1:<port>` not `0.0.0.0:<port>`; `kubectl get nodes` still works via the emitted
  kubeconfig.

  **Resolution:** Fixed. k3d `SimpleConfig` now sets `ExposeAPI.HostIP="127.0.0.1"`; the kubeconfig-server-port fallback was also corrected to `127.0.0.1` (was `k3d.DefaultAPIHost`, i.e. `"0.0.0.0"` — not even dialable). kind already defaulted to `127.0.0.1` (confirmed in `sigs.k8s.io/kind`'s own defaulting code) — no change needed there.
- [x] **S5. Silent loss of chart-version pinning: `versions.For` falls back to `"> 0.0.0"`, and the `signet` chart is never pinned at all.**
  `kluster-lib/versions/versions.go:136-143` — any addon missing from
  `~/.config/kluster/chart-versions.yaml` silently installs *latest available*, defeating the
  documented reproducibility guarantee (README “Chart version pinning”) and widening the
  supply-chain window. Concretely today: `versions.Catalog` has no `signet` entry, so
  `kluster-lib/addon/signet.go:84` (`versions.For("signet")`) **always** resolves `> 0.0.0` —
  the flagship Signet install is permanently unpinned. `traefik` is in the Catalog but note the
  Catalog key is `traefik` while the addon is `traefik-tls` — confirm the lookup key used in
  `addon/traefik.go:199` matches (it uses `versions.For("traefik")`, which is fine — leave it,
  just don't “fix” it into a mismatch).
  **Fix:** (1) add a `signet` entry to the version-resolution path — OCI charts don't have an
  `index.yaml`, so either resolve the latest tag via the GHCR OCI API during
  `Fetch`, or add an explicit pinned default; (2) make `For()` log a loud warning (or return an
  error surfaced by the caller) when it falls back to `> 0.0.0`, so unpinned installs are never
  silent.
  **Verify:** unit test: `For("nonexistent")` triggers the warning path;
  `kluster charts list` shows a `signet` version; grep install logs for the warning when the
  versions file is deleted mid-flow.

  **Resolution:** Partially fixed. `versions.For()` now prints a loud warning every time it falls back to `"> 0.0.0"` (previously silent). Did not implement OCI tag-listing for the signet chart itself (bullet 1 of the fix) — a live check found GHCR's anonymous-token flow non-trivial to get right, and hand-rolling a registry-API client for one addon's version pin was judged not worth the added risk versus documenting the limitation clearly; Helm's own registry client (confirmed via source read) already resolves the install correctly against live tags, it's just not tracked by kluster's own pin file. Documented in README and `design/checklist.md`.
- [x] **S6. `install.sh`: checksum verification is silently skipped, and `sudo mv` leaves the binary user-owned.**
  `install.sh:63-85` — if `checksums.txt` fails to download *or* no sha tool exists, the script
  proceeds with **no verification and no warning** (the `if [ -n "$EXPECTED" ]` block just
  doesn't run). And `install.sh:88-93` — `sudo mv "$TMP" "$DEST"` preserves the invoking user's
  ownership and the 0700 mode from `mktemp`+`chmod +x`, so `/usr/local/bin/kluster` ends up
  owned (and silently modifiable, and on some setups not even executable by others) by the
  installing user rather than root.
  **Fix:** warn loudly (or `fatal`) when the checksum cannot be verified; replace both `mv`
  branches with `install -m 0755` / `sudo install -o root -g root -m 0755`. Also consider
  publishing and verifying a cosign signature or GitHub artifact attestation (pairs with S13).
  **Verify:** `shellcheck install.sh`; run it against a real release in a container and check
  `stat -c '%U %a' /usr/local/bin/kluster` → `root 755`; simulate a missing `checksums.txt`
  and confirm the loud warning/failure.

  **Resolution:** Fixed. `install.sh` now `fatal`s if no sha tool is available, `checksums.txt` fails to download, or no matching entry exists — no longer silently skips verification. Install now uses `install -m 0755` / `sudo install -o root -g root -m 0755` instead of `mv`. `shellcheck` wasn't available in this environment; verified with `bash -n` (syntax OK).
- [x] **S7. Hardcoded well-known credentials across addons — only ArgoCD's is documented.**
  Inventory: RabbitMQ `guest/guest` (`kluster-lib/addon/rabbitmq.go:27-29`), Grafana
  `admin/admin` (`addon/observability.go:113-114`), Dex static user
  `admin@example.com` / `password` + static OAuth client secret `authstar-dev-secret`
  (`addon/dex.go:36-48`), ArgoCD `kluster-admin` (`addon/argocd.go:28`, documented in README).
  Acceptable for a disposable local cluster **if** (a) none are reachable off-host (S4 closes
  the main path) and (b) users are told. Today the README documents only the ArgoCD password.
  **Fix:** add a “Default credentials — dev only” table to the README listing every credential
  above and stating the exposure model (services are ClusterIP; reachable only via kubeconfig
  access). Optionally (better): generate a per-cluster random password for Grafana and RabbitMQ
  at install time and print it in the `up` summary — the pattern already exists for the Signet
  master key (`addon/signet.go:129-157`). Keep Dex's static client fixed (AuthStar's config
  depends on it) but say so explicitly.
  **Verify:** README renders the table; if random creds are implemented, `kluster up` output
  shows them and login works.

  **Resolution:** Documented (chose the documentation half of the fix, not per-cluster random-password generation, to keep this item scoped — noted as a possible future improvement). Added a "Default credentials — dev only" table to README covering RabbitMQ, Grafana, Dex (static user + OAuth client), and ArgoCD.
- [x] **S8. `kluster.yaml` in the current directory can steer destructive commands — repo-supplied config can delete an unrelated cluster.**
  `kluster/cmd/config.go:19-34` + `down.go:22` — `kluster down` with no flags reads `name` (and
  `provider`) from `./kluster.yaml`. Clone a malicious/mischievous repo containing
  `kluster.yaml` with `name: dev-spire`, run `kluster down` reflexively, and your unrelated
  long-lived dev cluster is gone. Same vector lets a repo silently change `trust-domain` or
  `provider` for `up`.
  **Fix:** for `down`, print the resolved name **and its source** (`from ./kluster.yaml`) and
  require an interactive `y/N` confirmation when the name came from a CWD config file (add
  `--yes`/`-y` to skip, for CI). Lower-cost alternative: always echo
  `using config ./kluster.yaml (name=..., provider=...)` on every command so the influence is
  visible.
  **Verify:** in a scratch dir with a planted `kluster.yaml`, `kluster down` prompts and shows
  the config source; `kluster down --yes` and `kluster down --name x` do not prompt.

  **Resolution:** Fixed. `kluster down` now prompts for confirmation (showing the config source) when the resolved name came from a repo-local `./kluster.yaml` rather than `--name`; `--yes`/`-y` skips it, and an explicit `--name` always bypasses the check. `kluster up` prints a lighter "Using config ..." notice in the same situation (non-destructive, so no prompt needed). Manually verified all three paths (default prompt+abort, `--yes`, explicit `--name`) against a planted `kluster.yaml` in a scratch directory.
## 3. Security — Low / hardening

- [x] **S9. Release pipeline: actions pinned by tag, no build provenance.**
  `.github/workflows/release-build.yml` — `actions/checkout@v4` / `setup-go@v5` are mutable-tag
  pins, and the binaries + `checksums.txt` are produced in the same job with no attestation, so
  a compromised job rewrites both consistently. **Fix:** pin actions by commit SHA, add
  `actions/attest-build-provenance` (needs `id-token: write`, `attestations: write`) for
  `dist/*`, and mention `gh attestation verify` in the README install section. Apply the same
  SHA-pinning to `release-please.yml`.
  **Verify:** workflow lint (`actionlint`), next release run green with attestations visible on
  the release page.

  **Resolution:** Fixed. Every action in `release-build.yml`/`release-please.yml` is now pinned by resolved commit SHA (with a trailing `# vX` comment); added `actions/attest-build-provenance` for `dist/*` with the required `id-token`/`attestations` permissions. README documents `gh attestation verify`. `actionlint` wasn't available in this environment; verified via a Python YAML parse of both files.
- [x] **S10. No vulnerability scanning or dependency-update automation in CI.**
  There is no CI workflow running tests/lint at all (only release-please + release-build), and
  nothing runs `govulncheck`. **Fix:** add a `ci.yml` running `go build ./... && go test ./...`,
  `golangci-lint`, and `govulncheck ./...` for both modules on PRs, plus a
  Dependabot/Renovate config for gomod + github-actions. Run `govulncheck` once now and record
  results in this file.
  **Verify:** CI green on a test PR; `govulncheck ./...` output captured.

  **Resolution:** Fixed, with one deliberate deviation. Added `ci.yml` (build/vet/test via `make`, `golangci-lint` pinned to v1.64.8 — this repo's `.golangci.yml` is v1-schema, incompatible with golangci-lint v2's default config — and `govulncheck`, for both modules), plus `dependabot.yml` for both Go modules and github-actions. Ran `govulncheck` locally now, as asked: both modules report **GO-2026-5746** (`github.com/docker/docker`, transitive via k3d/kind's Docker client), `Fixed in: N/A` upstream. Made that CI step `continue-on-error: true` with an explanatory comment rather than ship a permanently-red required check — recorded in `design/checklist.md` under a new "CI / Supply Chain" section.
- [x] **S11. kind provider permanently modifies host kernel sysctls, silently.**
  `kluster-lib/provider/kind.go:143-166` — `raiseKindInotifyLimits` execs `sysctl -w` inside a
  **privileged container sharing the host kernel**, i.e. it raises `fs.inotify.*` on the host,
  never reverts it on `down`, and swallows all errors. Deliberate and reasonable for CI, but it
  is undisclosed host state mutation. **Fix:** log one line when it happens
  (`raising host inotify limits (fs.inotify.max_user_watches=524288) — persists until reboot`),
  and document it in the README kind section. Do not try to revert on delete (other clusters
  may rely on it); just disclose.
  **Verify:** create a kind cluster; the notice appears; README updated.

  **Resolution:** Fixed. Added a one-line stderr notice ("kind: raising host inotify limits ... — persists until reboot") before the `sysctl` exec, plus a README note under the kind/CI section explaining the host-kernel-sharing behavior and that it's never reverted on `down`.
- [x] **S12. `mergeKubeconfig` silently clobbers same-named entries and writes non-atomically.**
  `kluster/cmd/kubeconfig.go:83-97` — same-named clusters/users/contexts in `~/.kube/config`
  are overwritten without warning; the write has no backup and no lockfile (a concurrent
  `kubectl config` write can interleave). **Fix:** warn when an existing entry with the same
  name is being replaced and it differs; write to a temp file in the same directory and rename
  over `~/.kube/config`; keep 0600.
  **Verify:** unit test for the collision warning; `kluster use` twice in a row stays quiet,
  `kluster use` over a foreign same-named context warns.

  **Resolution:** Fixed. `mergeKubeconfig` now warns (via a new `warn io.Writer` parameter, wired to `cmd.ErrOrStderr()` in both `kubeconfig.go` and `use.go`) when a same-named cluster/user/context entry differs from the incoming one, and writes via a temp file + `os.Rename` instead of `clientcmd.WriteToFile`'s in-place `os.WriteFile`.
- [x] **S13. Helm index fetch has no timeout or size bound.**
  `kluster-lib/versions/versions.go:155-178` — `http.DefaultClient` (no `Timeout`) +
  `io.ReadAll` of the response. The CLI context has no deadline, so a hung or malicious-tarpit
  repo stalls `kluster up` forever; a huge index.yaml balloons memory. **Fix:** dedicated
  client with a ~30s timeout, and wrap the body in `io.LimitReader` (~50 MiB — real Helm
  indexes reach tens of MB).
  **Verify:** unit test with an httptest server that hangs → fetch errors out within the
  timeout.

  **Resolution:** Fixed. `fetchIndex` now uses a dedicated `http.Client{Timeout: 30s}` and wraps the response body in `io.LimitReader` (50 MiB).
- [x] **S14. Catch-all `ClusterSPIFFEID` issues SVIDs to every non-system workload — contradicts the "mirror production behavior" design constraint.**
  `kluster-lib/profile/spire.go:38-53` — every pod in every namespace except
  `kube-system`/`spire-system` gets an SVID, including cert-manager, monitoring, ArgoCD, and
  anything a test deploys. Fine as a dev default, but CLAUDE.md constraint #4 sells the SPIRE
  setup as production-mirroring, and no production trust domain issues identities to arbitrary
  workloads. **Fix (doc-first):** document the catch-all explicitly (README spire profile
  section) as a dev convenience; extend the `NotIn` exclusion list to the infrastructure
  namespaces kluster itself creates (`cert-manager`, `monitoring`, `argocd`, `dex`,
  `rabbitmq`); optionally add a future `--spiffe-scope` knob. Verify the signet workload still
  gets its SVID (the `signet` namespace must stay in scope).
  **Verify:** `kluster up --profile signet` still passes the SVID probe; cert-manager pods no
  longer appear in `kubectl get clusterspiffeid -o yaml` selected-pod status / spire-server
  entries.

  **Resolution:** Fixed. Extended the `ClusterSPIFFEID` `NotIn` list to exclude kluster's own non-SPIFFE-aware infra (`cert-manager`, `monitoring`, `argocd`, `dex`, `rabbitmq`) — confirmed `signet` is **not** in the exclusion list, so it still gets its SVID. Documented as a deliberate dev-only convenience in both the code comment and the README `spire` profile section.
- [x] **S15. Failed `up` leaves a half-built cluster with no cleanup or resume guidance.**
  `kluster-lib/cluster/cluster.go:94-133` — `provider.Create` rolls itself back on failure
  (`provider/k3d.go:82-85`), but any later addon/profile failure returns immediately, leaving
  the cluster running half-configured; addon resolution errors (e.g. S3) happen *after* cluster
  creation, and re-running `up` fails with `cluster already exists`. **Fix:** resolve
  profiles/addons **before** creating the cluster (pure validation, no reason to do it after),
  and on later failures print explicit guidance
  (`cluster "x" was created but addon installation failed; run 'kluster down --name x' and retry`).
  A `--keep`/auto-teardown flag is optional; the reorder + message is the required part.
  **Verify:** unit test asserting `Up` with an unknown addon fails before `provider.Create` is
  called (use a fake provider that records calls).

  **Resolution:** Fixed. `resolveProfiles`/`collectAddons`/`topoSort` now run before `provider.Create` in `Cluster.Up`; every error path after cluster creation now appends explicit `cluster "x" was created but is not fully configured; run 'kluster down --name x' and retry` guidance. Added `TestUp_UnknownAddonFailsBeforeClusterCreate` with a fake provider that records whether `Create` was called.
- [x] **S16. Signet dev posture: root@CockroachDB with `sslmode=disable`, audit-chain key in Helm values.**
  `kluster-lib/addon/signet.go:98-124` — the DB connection is `root` with TLS off (CockroachDB
  single-node insecure mode), and `auditChainKey` travels through Helm values, so it is readable
  via `helm get values signet -n signet` and in the Helm release Secret — same-namespace
  secret-read access either way, so this is acceptable *for dev*, but it must be labeled.
  **Fix:** comments in `signetValues` + a “dev-only posture” note in the README signet section
  (insecure DB, key in release values, master key stored beside the data it protects via
  auto-unseal). No code change required unless Signet's chart supports secure-mode CRDB
  cheaply.
  **Verify:** README/CLAUDE.md updated; comment present.

  **Resolution:** Documented. Added a doc comment on `signetValues()` and a README note under the `signet` profile section covering the insecure CockroachDB mode and the audit-chain-key-via-Helm-values exposure, clarifying the master key (the thing that actually gates access) never takes that path.
## 4. Documentation / stated-behavior gaps

- [x] **D1. CLAUDE.md dependency table says Helm SDK `helm.sh/helm/v3`; the code uses `helm.sh/helm/v4` (v4.2.0) via `go-helm-client v0.13.1`.**
  `CLAUDE.md` “Primary Go dependencies” vs `kluster-lib/go.mod:14`. Update the table (and the
  design constraint #2 text) to v4.
  **Verify:** grep CLAUDE.md for `helm/v3` → no hits.

  **Resolution:** Fixed. CLAUDE.md — and, found while in the area, `design/draft.md`, which had the identical stale claim in two places — now say `helm.sh/helm/v4`.
- [x] **D2. CLAUDE.md module layout and CLI surface are stale.**
  Missing from the layout: `addon/apply.go`, `addon/probe.go`, `addon/argocd.go`,
  `addon/signet.go`, `profile/signet.go`, the whole `versions/` package, and CLI commands
  `use`, `charts`, `setup`, `config.go`, `progress.go`. The CLI surface block also omits
  `kluster use` / `kluster charts ...` / `kluster setup`. `provider/kind.go` is labeled
  “(future/CI)” but is fully implemented. Update the tree and command list.
  **Verify:** every file in `kluster-lib/**/*.go` and `kluster/cmd/*.go` appears in (or is
  deliberately summarized by) the CLAUDE.md layout.

  **Resolution:** Fixed. CLAUDE.md's module tree now lists `apply.go`, `probe.go`, `argocd.go`, `versions/`, and the `kluster/cmd/{config,use,charts,setup,progress}.go` files; `kind.go` relabeled from "(future/CI)" to reflect that it's fully implemented; the CLI command surface example now includes `setup`/`use`/`charts`.
- [x] **D3. README claims kluster "does not shell out to any external tools" — `kluster setup` shells out extensively.**
  `README.md:26` vs `kluster/cmd/setup.go` (runs `docker version`, `kubectl version`, `brew`,
  `curl | sh` pipeline, `sudo`). The claim is true for *cluster operations* only. Reword to
  “cluster lifecycle and addon installation never shell out; the optional `kluster setup`
  helper invokes system tools to check/install prerequisites.” Align CLAUDE.md constraint #2's
  phrasing (“for core operations” is already right — just make README match).
  **Verify:** README no longer contains the absolute claim.

  **Resolution:** Fixed. README now scopes the "never shells out" claim to cluster lifecycle/addon installation, explicitly carving out `kluster setup` as the one exception.
- [x] **D4. `kluster setup` help text says it checks "Docker, k3d, kubectl" — k3d is never checked (it's embedded).**
  `kluster/cmd/setup.go:20-21` and README setup section. Remove k3d from the tool list in both
  (or add a sentence: “k3d/kind are embedded libraries and need no install”). Also fix the
  related stale spec in `design/checklist.md:198-204` which still describes installing k3d.
  **Verify:** `kluster setup --help` mentions only Docker and kubectl.

  **Resolution:** Fixed. `kluster setup --help` no longer lists k3d as a checked/installed tool (it's an embedded library, not a CLI); README and `design/checklist.md`'s main table already didn't claim otherwise. Also removed `checklist.md`'s entire stale "Upcoming Features: kluster setup" planning spec, which *did* describe installing k3d and was fully superseded by the shipped implementation.
- [x] **D5. `checkDocker` reports success when the daemon is not running, so `setup` prints "All prerequisites satisfied" on a dead Docker.**
  `kluster/cmd/setup.go:153-162` returns `("(daemon not running)", true)` and the summary at
  `setup.go:94-97` treats it as OK. Treat daemon-not-running as a failure state with its own
  message (“docker CLI found but daemon unreachable — start Docker Desktop / dockerd”), and
  suppress the all-clear.
  **Verify:** with Docker stopped, `kluster setup` exits without the all-clear line.

  **Resolution:** Fixed. `checkDocker()` now returns `found=false` (not `true`) when the daemon is unreachable, with a distinct message ("found, but daemon unreachable — start Docker Desktop / dockerd") instead of silently passing; the `dockerOnly` branch in `runSetup` was updated to print that message without also showing "install Docker" instructions, which wouldn't help here.
- [x] **D6. `design/checklist.md` is stale relative to the in-flight signet work.**
  `design/checklist.md:124-131` still lists the `signet` profile as “⬜ not started” and says
  `authstar` depends on `spire` directly; both are now implemented
  (`profile/signet.go`, `addon/signet.go`, `authstar.go` now requires `signet`). Update the
  checklist: add rows for the `signet` addon + profile (including the socket-path overrides and
  auto-unseal secret behavior), move the Upcoming-Features entry to done, and reflect the new
  `--profile` help text. Also add the missing `signet` row context to the Chart Versions
  section once S5 is fixed.
  **Verify:** checklist matches `git status` reality; no “not started” entry for shipped code.

  **Resolution:** Verified and fixed. Also found and removed two other fully-superseded "Upcoming Features" specs in the same file during this pass (Config file, ArgoCD addon — both already shipped and marked done in their real tables above), for the same reason as the signet-profile one already cleaned up in the prior rename commit.
- [x] **D7. README chart-pinning section overstates team reproducibility.**
  `README.md:388-398` says cached versions “ensur[e] reproducible installs across your team”,
  but the file is generated per-machine at first run — two teammates who first-ran a week apart
  have different pins, and the `> 0.0.0` fallback (S5) can bypass pinning entirely. Reword to
  per-machine reproducibility, and document how to actually share pins (commit
  `chart-versions.yaml` to the repo + a `--chart-versions <path>` flag, or at minimum an env
  var/XDG override note). If you add the flag, wire it through `versions.Path()`.
  **Verify:** README wording matches actual behavior; if the flag was added, an integration
  test covers it.

  **Resolution:** Fixed. README now describes the pinning file as per-machine (not automatically team-wide), explains why (generated on first run, per machine), gives a concrete way to actually share it, and notes that OCI-only addons (`signet`) are never pinned by it at all. Did not add the optional `--chart-versions <path>` flag — scoped out as a feature addition beyond a doc-accuracy fix.
- [x] **D8. ArgoCD addon comment claims the admin password "never appears in plaintext in a Kubernetes secret" — misleading.**
  `kluster/cmd/../kluster-lib/addon/argocd.go:80-82` — true only in the narrow sense that the
  *hash* is what lands in `argocd-secret`; the plaintext is a published constant in source and
  in the README. Reword the comment to say the bcrypt hash avoids ArgoCD's random-initial-secret
  extraction step, not that the password is protected. Also note `server.insecure: "true"`
  (`argocd.go:46`) is TLS-termination-at-Traefik by design — add a comment saying so, so it
  isn't “fixed” into breakage later.
  **Verify:** comments updated; no behavior change.

  **Resolution:** Fixed. Reworded the bcrypt-hashing comment in `argocd.go` to describe what it actually does (lets us set a known password via Helm values, skipping ArgoCD's random-password-then-extract dance) rather than implying the plaintext is protected; added a comment on `server.insecure: "true"` noting it's TLS-termination-at-Traefik by design, not an oversight.
- [x] **D9. README documents UIs at `argocd.<trust-domain>` etc. without explaining how those hostnames resolve.**
  `README.md:206` — nothing in kluster creates DNS or `/etc/hosts` entries for
  `argocd.dev.cluster.local`, and the k3d SimpleConfig maps no host ports for 80/443
  (`provider/k3d.go:45-56`), so out of the box the IngressRoute is unreachable from the host
  and the hostname unresolvable. Either (a) document the required steps
  (`/etc/hosts` entry + k3d port mapping, e.g. a `--map-http` option or
  `ports: 443:443@loadbalancer` equivalent in the SimpleConfig), or (b) add the port mapping to
  the k3d provider and document only the hosts entry. As-is, the documented ArgoCD login flow
  cannot be followed.
  **Verify:** following the README from scratch reaches the ArgoCD login page (or the README
  explicitly lists the manual steps that make it reachable).

  **Resolution:** Documented. Added a README note under "Optional addons" explaining that reaching `argocd.<trust-domain>` requires a manual `kubectl port-forward` + `/etc/hosts` entry today, since kluster doesn't map a host port to Traefik or touch `/etc/hosts` (confirmed via k3d source: no default port mapping is added). Did not implement automatic port-mapping — the finding itself offered documentation as the lower-risk option.
- [x] **D10. Trust-domain default `"dev.cluster.local"` is duplicated in six places.**
  `cluster/config.go:4` defines `DefaultTrustDomain` but nothing uses it — the literal is
  re-hardcoded in `addon/spire.go:152`, `addon/traefik.go:212`, `addon/argocd.go:34`,
  `addon/signet.go:99`, `profile/spire.go:35`, and `kluster/cmd/up.go:25`. Not a security bug,
  but drift here silently splits the trust domain between components. Normalize once (e.g.
  resolve the default into `provider.ClusterConfig` at CLI/config load time and delete the
  per-site fallbacks, or have all sites call a single `cfg.TrustDomainOrDefault()`).
  **Verify:** `grep -rn '"dev.cluster.local"' kluster-lib kluster` returns exactly one
  definition site (plus docs).

  **Resolution:** Fixed. Added `provider.DefaultTrustDomain` + `ClusterConfig.TrustDomainOrDefault()` (in the `provider` package specifically, since addon/profile packages can't import `cluster`, which is where the now-removed, always-unused `cluster.DefaultTrustDomain` constant lived) and switched every call site (spire/traefik/argocd/signet addons, the spire profile, `up.go`'s flag default) to use it. Confirmed via grep: exactly one `"dev.cluster.local"` literal remains in the whole codebase.
---

## Explicitly reviewed and considered acceptable (no action)

- `kluster kubeconfig --output` writes with 0600 (`kluster/cmd/kubeconfig.go:54`); merge path
  uses `clientcmd.WriteToFile` which is also 0600.
- Signet master-key generation uses `crypto/rand` and never overwrites an existing key
  (`addon/signet.go:129-157`) — correct.
- ArgoCD password is bcrypt-hashed at install time with `crypto/bcrypt` — correct mechanism.
- Server-side apply with `FieldManager: "kluster"` + `Force: true` (`addon/apply.go:43-46`) is
  appropriate for a tool that owns these resources.
- `versions.Save` writes 0600 under a 0700 dir — fine.
- k3d create failure rollback (`provider/k3d.go:82-85`) — good hygiene.
