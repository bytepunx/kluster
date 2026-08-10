// RabbitMQ per-service credential provisioning Job — authstar ADR 0012
// (~/git/authstar/design/decisions/0012-rabbitmq-user-provisioning-job.md).
//
// Runs as a one-shot Kubernetes Job (MODE=bootstrap, wired as a post-install
// step by kluster's authstar profile) or a daily CronJob (MODE=rotate).
// For each service in TARGET_SERVICES:
//   1. generate a random RabbitMQ credential
//   2. create/update a Kubernetes Secret holding it, and rabbitmq.com/v1beta1
//      User + Permission CRDs so the RabbitMQ Messaging Topology Operator
//      provisions the actual broker user, vhost-scoped and least-privilege
//   3. push the same credential into that service's own signet bundle via
//      SyncBundle, SOPS-encrypted (signet only ever accepts SOPS ciphertext
//      over this RPC)
//
// Per ADR 0012's rotation-safe ordering, step 2 (the RabbitMQ side) always
// happens before step 3 (signet) — bounding the exposure window on
// MODE=rotate re-runs to "a consumer that happens to reconnect between the
// operator applying the new credential and the consumer's own next signet
// bundle fetch", rather than leaving the ordering undefined.
//
// This Job's own elevated GitOpsService admin token is fetched from its
// *own* signet bundle (OWN_NAMESPACE/OWN_SERVICE) via SPIFFE-mTLS workload
// access — kluster seeds that token ahead of time (see
// kluster-lib/profile/authstar.go), standing in for ADR 0012's "provisioned
// once by a human operator" step, the same way kluster's own RabbitMQ
// bootstrap credential is seeded in kluster-lib/addon/rabbitmq.go.
import * as k8s from "@kubernetes/client-node";
import { status as grpcStatus } from "@grpc/grpc-js";
import { dialWorkload, gitOpsClient } from "@bytepunx/signet-client";

import { sopsEncryptValue, tarGzSingleFile, randomToken } from "./lib.js";

const MODE = process.env.MODE ?? "bootstrap";
const OWN_NAMESPACE = process.env.OWN_NAMESPACE ?? "rabbitmq";
const OWN_SERVICE = process.env.OWN_SERVICE ?? "rabbitmq-provisioner";
const TRUST_DOMAIN = required("TRUST_DOMAIN");
const SIGNET_WORKLOAD_ADDR = process.env.SIGNET_WORKLOAD_ADDR ?? "signet.signet.svc.cluster.local:8443";
// signetd's admin gRPC listener (GitOpsService — GetSOPSPublicKey,
// SyncBundle) is reached directly over this in-cluster Service, not a
// Kubernetes port-forward. An earlier version tunneled via
// pods/portforward, which needed extra RBAC and turned out to hit a
// persistent 403 on the WebSocket upgrade @kubernetes/client-node's
// PortForward class uses; see kluster-lib/addon/signet_admin.go's package
// doc comment and kluster-lib/addon/signet.go's ensureSignetAdminService
// for the full story of why a plain Service is both simpler and doesn't
// weaken signetd's actual access control (still a bearer token on every
// RPC, checked in getSopsPublicKey/pushServiceSecret below exactly the
// same as before).
const SIGNET_ADMIN_ADDR = process.env.SIGNET_ADMIN_ADDR ?? "signet-admin.signet.svc.cluster.local:8444";
const SPIFFE_SOCKET = process.env.SPIFFE_SOCKET ?? "/run/spire/sockets/spire-agent.sock";
const RABBITMQ_NAMESPACE = process.env.RABBITMQ_NAMESPACE ?? "rabbitmq";
const RABBITMQ_CONNECTION_SECRET = process.env.RABBITMQ_CONNECTION_SECRET ?? "rabbitmq-topology-operator-connection";
const RABBITMQ_VHOST = process.env.RABBITMQ_VHOST ?? "authstar";
const TARGET_SERVICES = (process.env.TARGET_SERVICES ?? "tower,keep")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean);

const RABBITMQ_GROUP = "rabbitmq.com";
const RABBITMQ_API_VERSION = "v1beta1";
// v1.20.0 of the Messaging Topology Operator requires every user-provided
// Secret it reads (connectionSecret, importCredentialsSecret, ...) to carry
// this label, enforced by an admission webhook — see the v1.20.0 release
// notes' "IMPORTANT NOTICE" on the breaking secret-label requirement.
const TOPOLOGY_OPERATOR_SECRET_LABEL = { "rabbitmq.com/topology-operator": "true" };

function required(name) {
  const v = process.env[name];
  if (!v) throw new Error(`missing required env var ${name}`);
  return v;
}

function log(...args) {
  console.log(`[rabbitmq-provisioner:${MODE}]`, ...args);
}

async function main() {
  const kc = new k8s.KubeConfig();
  kc.loadFromCluster();
  const coreV1 = kc.makeApiClient(k8s.CoreV1Api);
  const customObjects = kc.makeApiClient(k8s.CustomObjectsApi);

  log(`fetching own signet bundle (${OWN_NAMESPACE}/${OWN_SERVICE}) for admin token`);
  const adminToken = await fetchOwnAdminToken(kc);

  log(`dialing signet admin API at ${SIGNET_ADMIN_ADDR}`);
  // SIGNET_ADMIN_ADDR is a real in-cluster DNS name
  // (signet-admin.signet.svc.cluster.local), not a loopback address, but
  // signetd's admin listener is still plaintext by design (see
  // kluster-lib/addon/signet.go's ensureSignetAdminService doc comment:
  // the bearer token on every RPC is the actual access control, not
  // transport encryption). gitOpsClient()'s default TLS-vs-plaintext
  // heuristic only trusts loopback addresses with plaintext, so the
  // explicit `plaintext: true` override (bytepunx/signet-clients#32) is
  // required here — without it this would pick TLS and signetd would
  // reject the handshake outright ("wrong version number"). This used to
  // require bypassing gitOpsClient() entirely with a hand-built
  // GitOpsServiceClient; the plaintext option landed upstream instead
  // (bytepunx/kluster#20 tracks reverting workarounds like this one).
  const gitops = gitOpsClient({ address: SIGNET_ADMIN_ADDR, token: adminToken, plaintext: true });
  try {
    const sopsKey = await getSopsPublicKey(gitops);
    log(`signet SOPS age public key: ${sopsKey.slice(0, 20)}...`);

    for (const service of TARGET_SERVICES) {
      await provisionService({ coreV1, customObjects, gitops, sopsKey, service });
    }
  } finally {
    gitops.close();
  }

  log("done");
}

// SPIRE's controller-manager registers a workload's SPIFFE identity
// per-Pod, reacting to the pod's own creation — that registration needs a
// few seconds to reach the node-local SPIRE agent this process dials over
// the CSI socket, and a brand-new ServiceAccount's very first pod can race
// it and see "no identity issued" from the workload API. dialWorkload
// retries this internally by default now (bytepunx/signet-clients#33; up
// to 5 attempts, 1s/2s/4s/8s backoff, only for that specific failure), so
// this Job no longer needs its own retry wrapper around it — see
// bytepunx/kluster#20 for tracking workarounds like the one this replaced.
// This Job's pod spec still sets restartPolicy: OnFailure (see
// kluster-lib/profile/authstar.go's rabbitmqProvisionerPodSpec doc
// comment) as a second layer in case the library's own bounded retry is
// exhausted before SPIRE catches up.
async function fetchOwnAdminToken(kc) {
  const { client, close } = await dialWorkload({
    address: SIGNET_WORKLOAD_ADDR,
    workloadSocket: `unix://${SPIFFE_SOCKET}`,
    trustDomain: TRUST_DOMAIN,
  });
  try {
    const bundle = await new Promise((resolve, reject) =>
      client.getServiceBundle({ namespace: OWN_NAMESPACE, service: OWN_SERVICE }, (err, r) =>
        err ? reject(err) : resolve(r),
      ),
    );
    const encoded = bundle.bundle?.secrets?.["signet-admin-token"];
    if (!encoded) {
      throw new Error(
        `no "signet-admin-token" secret in bundle ${OWN_NAMESPACE}/${OWN_SERVICE} — kluster's authstar profile should have seeded this before running the Job`,
      );
    }
    return Buffer.from(encoded, "base64").toString("utf8");
  } finally {
    close();
  }
}

// getSopsPublicKey mirrors kluster-lib/addon/signet_admin.go's
// ensureSOPSPublicKey: a brand-new signet install has no SOPS key at all
// until something calls RotateSOPSKey once (signet's own docs/tooling treat
// this as a separate, explicit provisioning step — see that Go function's
// doc comment for the full story). kluster's own RabbitMQ bootstrap-
// credential seeding (addon/rabbitmq.go) already bootstraps this key well
// before this Job ever runs, so in practice GetSopsPublicKey should always
// succeed here — this fallback exists so this image doesn't silently
// depend on that ordering if it's ever run standalone against a fresh
// signet.
function getSopsPublicKey(gitops) {
  return new Promise((resolve, reject) =>
    gitops.getSopsPublicKey({}, (err, resp) => {
      if (!err) return resolve(resp.publicKey);
      if (err.code !== grpcStatus.NOT_FOUND) return reject(err);
      gitops.rotateSopsKey({}, (rotateErr, rotateResp) =>
        rotateErr ? reject(rotateErr) : resolve(rotateResp.newPublicKey),
      );
    }),
  );
}

async function pushServiceSecret(gitops, namespace, service, secretName, value, sopsKey) {
  const encrypted = sopsEncryptValue(value, sopsKey);
  const archive = await tarGzSingleFile(`secrets/${namespace}/${service}/${secretName}.yaml`, encrypted);

  const responsePromise = new Promise((resolve, reject) => {
    const stream = gitops.syncBundle((err, resp) => (err ? reject(err) : resolve(resp)));
    stream.write({ header: { secretsPath: "secrets/" } });
    stream.write({ data: archive });
    stream.end();
  });

  const resp = await responsePromise;
  if (resp.errors?.length) {
    throw new Error(`SyncBundle reported errors for ${namespace}/${service}/${secretName}: ${resp.errors.join("; ")}`);
  }
}

// provisionService generates one fresh RabbitMQ credential for `service`
// and applies the RabbitMQ-side objects before pushing to signet (see the
// module doc comment on ordering).
async function provisionService({ coreV1, customObjects, gitops, sopsKey, service }) {
  log(`provisioning RabbitMQ credential for "${service}"`);

  const username = service;
  const password = randomToken(24);
  const credentialsSecretName = `${service}-rabbitmq-credentials`;

  await upsertSecret(coreV1, RABBITMQ_NAMESPACE, credentialsSecretName, {
    username,
    password,
  });

  await upsertUser(customObjects, service, credentialsSecretName);
  await upsertPermission(customObjects, service);

  if (MODE === "rotate") {
    await waitForUserReady(customObjects, service);
  }

  await pushServiceSecret(gitops, service, service, "rabbitmq-username", username, sopsKey);
  await pushServiceSecret(gitops, service, service, "rabbitmq-password", password, sopsKey);

  log(`provisioned "${service}": k8s User/Permission applied, credential pushed to signet ${service}/${service}`);
}

// --- Kubernetes object helpers --------------------------------------------

async function upsertSecret(coreV1, namespace, name, stringData) {
  const body = {
    metadata: { name, namespace, labels: TOPOLOGY_OPERATOR_SECRET_LABEL },
    stringData,
  };
  try {
    await coreV1.createNamespacedSecret({ namespace, body });
  } catch (err) {
    if (err?.code !== 409) throw err;
    await coreV1.replaceNamespacedSecret({ name, namespace, body });
  }
}

async function upsertUser(customObjects, service, credentialsSecretName) {
  const body = {
    apiVersion: `${RABBITMQ_GROUP}/${RABBITMQ_API_VERSION}`,
    kind: "User",
    metadata: { name: service, namespace: RABBITMQ_NAMESPACE },
    spec: {
      importCredentialsSecret: { name: credentialsSecretName },
      rabbitmqClusterReference: { connectionSecret: { name: RABBITMQ_CONNECTION_SECRET } },
      tags: [],
    },
  };
  await upsertCustomObject(customObjects, "users", service, body);
}

async function upsertPermission(customObjects, service) {
  const name = `${service}-permission`;
  const body = {
    apiVersion: `${RABBITMQ_GROUP}/${RABBITMQ_API_VERSION}`,
    kind: "Permission",
    metadata: { name, namespace: RABBITMQ_NAMESPACE },
    spec: {
      vhost: RABBITMQ_VHOST,
      user: service,
      permissions: { configure: ".*", write: ".*", read: ".*" },
      rabbitmqClusterReference: { connectionSecret: { name: RABBITMQ_CONNECTION_SECRET } },
    },
  };
  await upsertCustomObject(customObjects, "permissions", name, body);
}

// upsertCustomObject creates the object if absent. It does not attempt an
// update on conflict: MODE=bootstrap is documented (ADR 0012) to be a no-op
// against an already-provisioned user, and MODE=rotate never changes these
// spec fields (only the referenced credentialsSecret's contents change,
// which the operator watches and re-syncs on its own — see the User CRD's
// importCredentialsSecret field description), so there's nothing to patch.
async function upsertCustomObject(customObjects, plural, name, body) {
  try {
    await customObjects.createNamespacedCustomObject({
      group: RABBITMQ_GROUP,
      version: RABBITMQ_API_VERSION,
      namespace: RABBITMQ_NAMESPACE,
      plural,
      body,
    });
    log(`created ${plural}/${name}`);
  } catch (err) {
    if (err?.code !== 409) throw err;
    log(`${plural}/${name} already exists, leaving as-is`);
  }
}

async function waitForUserReady(customObjects, service, timeoutMs = 60_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const obj = await customObjects.getNamespacedCustomObject({
      group: RABBITMQ_GROUP,
      version: RABBITMQ_API_VERSION,
      namespace: RABBITMQ_NAMESPACE,
      plural: "users",
      name: service,
    });
    const ready = obj.status?.conditions?.some((c) => c.type === "Ready" && c.status === "True");
    if (ready) return;
    await new Promise((r) => setTimeout(r, 2000));
  }
  log(`WARNING: User/${service} did not report Ready within ${timeoutMs}ms; proceeding anyway`);
}

// @kubernetes/client-node's CoreV1Api/CustomObjectsApi clients (used
// un-closed throughout this file) hold a keep-alive HTTP(S) agent open, so
// the process never drains its event loop naturally on success — only the
// error path used to call process.exit, which meant a *successful* run
// left the Job's pod running forever. kluster's own waitForJobComplete
// polls for the Job to report Complete (see kluster-lib/profile/authstar.go),
// so a process that never exits 0 hangs `kluster up` at that step
// indefinitely — confirmed for real: the pod sat at Running 0/1 for
// minutes after "done" had already logged, with only `node src/index.js`
// still alive inside it. Explicit process.exit(0) on success mirrors the
// existing explicit exit on failure below.
//
// keepAlive exists for the opposite problem, discovered the hard way after
// the above fix landed: @bytepunx/signet-client's dialWorkload (used by
// fetchOwnAdminToken, above) leaves the process with *nothing* holding the
// event loop open during its very first await — none of the K8s clients'
// handles exist yet at that point, and the SPIFFE Workload API stream
// dialWorkload waits on internally does not keep Node alive on its own
// (verified live: instrumenting every await with synchronous
// fs.appendFileSync debug lines showed execution consistently stopping
// mid-dialWorkload, with a clean process "exit" event firing at code 0 —
// no thrown error, no unhandled rejection, nothing — meaning Node
// considered the event loop empty and exited, abandoning that still-
// pending await entirely). A referenced (non-.unref()'d) interval spanning
// the whole main() call closes that gap deterministically regardless of
// what any dependency does with its own handles' ref state, and is
// cleared the instant main() actually settles either way, so it adds no
// delay to a real success or failure. Filed as
// bytepunx/signet-clients#47 upstream, since this is a real bug in
// dialWorkload itself and this is a workaround, not a fix.
const keepAlive = setInterval(() => {}, 60_000);
main()
  .then(() => {
    clearInterval(keepAlive);
    process.exit(0);
  })
  .catch((err) => {
    console.error(`[rabbitmq-provisioner:${MODE}] fatal:`, err);
    clearInterval(keepAlive);
    process.exit(1);
  });
