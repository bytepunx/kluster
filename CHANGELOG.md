# Changelog

## [1.2.1](https://github.com/bytepunx/kluster/compare/v1.2.0...v1.2.1) (2026-08-31)


### Bug Fixes

* set admin.tls.acknowledgeInsecure for signet addon Helm install ([#42](https://github.com/bytepunx/kluster/issues/42)) ([0522ae5](https://github.com/bytepunx/kluster/commit/0522ae5502cd48b07ae0704c6fb4ee920e12fbce)), closes [#40](https://github.com/bytepunx/kluster/issues/40)

## [1.2.0](https://github.com/bytepunx/kluster/compare/v1.1.0...v1.2.0) (2026-08-11)


### Features

* **rabbitmq-provisioner:** use signet-clients' native plaintext-dial and default retry ([#21](https://github.com/bytepunx/kluster/issues/21)) ([ff25d2d](https://github.com/bytepunx/kluster/commit/ff25d2deebe00984c6e2cfa8824e281df4bb977a))


### Bug Fixes

* **rabbitmq-provisioner:** use published signet-client 0.4.1; add missing src/lib.js ([#23](https://github.com/bytepunx/kluster/issues/23)) ([acd3981](https://github.com/bytepunx/kluster/commit/acd39815a2bc44678aa09423281c01dbff056860))

## [1.1.0](https://github.com/bytepunx/kluster/compare/v1.0.2...v1.1.0) (2026-07-18)


### Features

* add signet addon and profile ([7701aea](https://github.com/bytepunx/kluster/commit/7701aea22be10e8067e106cf67011465e80debfe))


### Bug Fixes

* address security and documentation-gap review ([38cc943](https://github.com/bytepunx/kluster/commit/38cc943422d0c60cdf674847a250f15448edf6da))
* correct lint tooling and outstanding format and lint issues ([d803cf0](https://github.com/bytepunx/kluster/commit/d803cf033725136cf1efd6ede8dd10a4742a7fe0))

## [1.0.2](https://github.com/bytepunx/kluster/compare/v1.0.1...v1.0.2) (2026-06-30)


### Bug Fixes

* trigger release build on published release instead of tag push ([9b866e8](https://github.com/bytepunx/kluster/commit/9b866e8317c13299a9f16375beb13e8f62cecad6))

## [1.0.1](https://github.com/bytepunx/kluster/compare/v1.0.0...v1.0.1) (2026-06-30)


### Bug Fixes

* correct argocd addon routing and readiness checks ([1b056f0](https://github.com/bytepunx/kluster/commit/1b056f052ae37a0cfbd4aac125cb133869dd1e0d))

## 1.0.0 (2026-06-25)


### Features

* initial release ([cc4854e](https://github.com/bytepunx/kluster/commit/cc4854e87b5235bf29847e1780579fc448e0727b))
