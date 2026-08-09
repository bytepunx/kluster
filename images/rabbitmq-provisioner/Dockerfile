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

FROM node:22-alpine

# signet's admin API only ever accepts SOPS ciphertext over SyncBundle (see
# src/lib.js's sopsEncryptValue doc comment) — Node has no maintained native
# SOPS/age implementation, so this image bundles the real upstream `sops`
# static binary and shells out to it, unlike kluster's own Go code (which
# uses github.com/getsops/sops/v3 as a library directly).
COPY --from=sops /usr/local/bin/sops /usr/local/bin/sops

WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev

COPY src/ ./src/

# Runs as a Kubernetes Job/CronJob, not a long-lived server — no EXPOSE,
# no health check. MODE/TRUST_DOMAIN/etc. are supplied via the Job/CronJob
# pod spec's env — see kluster-lib/profile/authstar.go.
ENTRYPOINT ["node", "src/index.js"]
