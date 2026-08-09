# syntax=docker/dockerfile:1
#
# rabbitmq-provisioner implements authstar ADR 0012: a one-shot Job
# (MODE=bootstrap) / daily CronJob (MODE=rotate) that generates per-service
# RabbitMQ credentials, pushes them into each service's own signet bundle,
# and creates the RabbitMQ Messaging Topology Operator's User/Permission
# CRDs so the broker actually gets that user. See src/index.js for the full
# flow and design rationale.
FROM node:22-alpine AS sops

ARG SOPS_VERSION=v3.13.3
ARG TARGETARCH
RUN apk add --no-cache curl && \
    curl -fsSL -o /usr/local/bin/sops \
      "https://github.com/getsops/sops/releases/download/${SOPS_VERSION}/sops-${SOPS_VERSION}.linux.${TARGETARCH}" && \
    chmod +x /usr/local/bin/sops

# @bytepunx/signet-client's TypeScript build isn't on npm yet at the version
# this image needs (0.4.0 -- the plaintext-admin-dial + default-workload-
# retry release, bytepunx/signet-clients#32/#33) because publishing is
# blocked on an npm Trusted Publisher misconfiguration, not a code issue --
# see bytepunx/kluster#20. Build it from source instead of installing from
# the registry: the repo commits its generated proto stubs (typescript/src/
# gen/), so no buf/protoc toolchain is needed here, just a plain TS build.
# SIGNET_CLIENTS_REF pins an exact commit for reproducibility; bump it (or
# drop this whole stage and go back to a normal npm dependency in
# package.json) once #20 is resolved.
FROM node:22-alpine AS signet-client-build
ARG SIGNET_CLIENTS_REF=b051350e05ac87914623f8780898f7b2a74e727e
RUN apk add --no-cache git
RUN git clone https://github.com/bytepunx/signet-clients.git /signet-clients && \
    git -C /signet-clients checkout "${SIGNET_CLIENTS_REF}"
WORKDIR /signet-clients/typescript
RUN npm ci
RUN npm run build
RUN mkdir /out && npm pack --pack-destination /out

FROM node:22-alpine

# signet's admin API only ever accepts SOPS ciphertext over SyncBundle (see
# src/lib.js's sopsEncryptValue doc comment) — Node has no maintained native
# SOPS/age implementation, so this image bundles the real upstream `sops`
# static binary and shells out to it, unlike kluster's own Go code (which
# uses github.com/getsops/sops/v3 as a library directly).
COPY --from=sops /usr/local/bin/sops /usr/local/bin/sops

WORKDIR /app
COPY package.json ./
COPY --from=signet-client-build /out/*.tgz ./signet-client.tgz
# package.json's "@bytepunx/signet-client" dependency is a "file:" reference
# to the tarball above, not a registry version -- see that stage's comment
# for why. A single npm install resolves both it and every other real
# registry dependency from package.json in one pass, so there's no risk of
# a second install silently re-fetching the registry version over it.
RUN npm install --omit=dev && rm signet-client.tgz

COPY src/ ./src/

# Runs as a Kubernetes Job/CronJob, not a long-lived server — no EXPOSE,
# no health check. MODE/TRUST_DOMAIN/etc. are supplied via the Job/CronJob
# pod spec's env — see kluster-lib/profile/authstar.go.
ENTRYPOINT ["node", "src/index.js"]
