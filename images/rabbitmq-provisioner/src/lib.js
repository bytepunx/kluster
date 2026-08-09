// Shared helpers for the RabbitMQ provisioning Job (ADR 0012). Split out of
// index.js purely for readability — nothing here is reused outside this
// image.
import { execFileSync } from "node:child_process";
import { writeFileSync, readFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomBytes } from "node:crypto";
import { createGzip } from "node:zlib";
import { Readable } from "node:stream";

// --- SOPS encryption -------------------------------------------------------
//
// signet's admin API only ever accepts SOPS ciphertext for secrets pushed
// via SyncBundle — confirmed directly from signet's own CLI source
// (cmd/signet/bundle_cmd.go: "Secrets are decrypted server-side using
// signet's age key, so only SOPS ciphertext ever leaves your machine.").
// Node has no maintained native SOPS/age implementation, unlike kluster's
// own Go code (which uses github.com/getsops/sops/v3 directly — see
// kluster-lib/addon/signet_admin.go's sopsEncryptValue) — so this image
// bundles the real `sops` CLI binary (see Dockerfile) and shells out to it,
// which is the pragmatic choice for this component specifically (unlike
// kluster's own Go binary, which deliberately avoids shelling out for core
// operations).
//
// The plaintext value is written to a temp file as `value: <plaintext>`
// (signet's own DecryptFile only recognises that exact top-level key —
// see internal/gitops/sops.go) and encrypted in place with `sops --encrypt
// --age <recipient>`, matching the byte-for-byte format every hand-authored
// secret file in this ecosystem already uses (see
// signet-smoke-test/secrets/*/*.yaml).
export function sopsEncryptValue(plaintext, ageRecipient) {
  const dir = mkdtempSync(join(tmpdir(), "sops-"));
  const path = join(dir, "secret.yaml");
  try {
    writeFileSync(path, `value: ${JSON.stringify(plaintext)}\n`, { mode: 0o600 });
    execFileSync("sops", ["--encrypt", "--age", ageRecipient, "--input-type", "yaml", "--output-type", "yaml", "--in-place", path]);
    return readFileSync(path);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// --- tar.gz single-file bundle archive --------------------------------
//
// Mirrors kluster-lib/addon/signet_admin.go's tarGzSingleFile and
// signet's own bundle_cmd.go: SyncBundle expects the header chunk followed
// by raw tar.gz bytes of a directory tree, "secrets/<namespace>/<service>/<name>.yaml".
export async function tarGzSingleFile(name, content) {
  // A hand-rolled minimal tar writer: one regular-file entry, no directory
  // entries (tar doesn't require parent directory entries to exist for a
  // reader to create them), padded to 512-byte blocks per the USTAR format,
  // followed by the two required 512-byte zero blocks marking end-of-archive.
  const nameBuf = Buffer.from(name, "utf8");
  if (nameBuf.length > 100) {
    throw new Error(`tar entry name too long for basic USTAR header: ${name}`);
  }
  const header = Buffer.alloc(512);
  nameBuf.copy(header, 0);
  header.write("0000600\0", 100, "utf8"); // mode
  header.write("0000000\0", 108, "utf8"); // uid
  header.write("0000000\0", 116, "utf8"); // gid
  header.write(content.length.toString(8).padStart(11, "0") + "\0", 124, "utf8"); // size (octal)
  header.write(Math.floor(Date.now() / 1000).toString(8).padStart(11, "0") + "\0", 136, "utf8"); // mtime
  header.write("        ", 148, "utf8"); // checksum placeholder (8 spaces)
  header.write("0", 156, "utf8"); // typeflag: regular file
  header.write("ustar\0", 257, "utf8"); // magic + version

  let checksum = 0;
  for (const b of header) checksum += b;
  header.write(checksum.toString(8).padStart(6, "0") + "\0 ", 148, "utf8");

  const padLen = (512 - (content.length % 512)) % 512;
  const padded = Buffer.concat([content, Buffer.alloc(padLen)]);
  const archive = Buffer.concat([header, padded, Buffer.alloc(1024)]);

  return new Promise((resolve, reject) => {
    const gz = createGzip();
    const chunks = [];
    gz.on("data", (c) => chunks.push(c));
    gz.on("end", () => resolve(Buffer.concat(chunks)));
    gz.on("error", reject);
    Readable.from(archive).pipe(gz);
  });
}

export function randomToken(bytes = 20) {
  return randomBytes(bytes).toString("hex");
}
