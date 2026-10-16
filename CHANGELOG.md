# Changelog

## [0.0.13](https://github.com/joangrigorov/rumil/compare/v0.0.12...v0.0.13) (2026-10-16)


### Bug Fixes

* name the permission thread resolution needs, and warn about it at startup ([#35](https://github.com/joangrigorov/rumil/issues/35)) ([f58f509](https://github.com/joangrigorov/rumil/commit/f58f50911f603d328e97731896350600979da3a6))

## [0.0.12](https://github.com/joangrigorov/rumil/compare/v0.0.11...v0.0.12) (2026-10-16)


### Features

* ask for a review in a comment, and acknowledge a request with a reaction ([#30](https://github.com/joangrigorov/rumil/issues/30)) ([5be575b](https://github.com/joangrigorov/rumil/commit/5be575bf404d62aed5cac24172d6250cc74bd0f5))
* browse and decide the recorded rules on a terminal ([#31](https://github.com/joangrigorov/rumil/issues/31)) ([e7635b6](https://github.com/joangrigorov/rumil/commit/e7635b6ad9d1138dbb6a40fb11f2405abb63012c))


### Bug Fixes

* decide a duplicate rule by its words, not only by its distance ([#29](https://github.com/joangrigorov/rumil/issues/29)) ([429e937](https://github.com/joangrigorov/rumil/commit/429e9377103f194c81c249441a54523e936c0fc6))
* hold app.bot_handle against the App's own slug ([#33](https://github.com/joangrigorov/rumil/issues/33)) ([18d387c](https://github.com/joangrigorov/rumil/commit/18d387c481f14d8963791bd7975bc6fbbf02f6d1))
* report GitHub's refusal to let an App close a review thread ([#34](https://github.com/joangrigorov/rumil/issues/34)) ([25808a0](https://github.com/joangrigorov/rumil/commit/25808a0378fa5cad9bb364a580081a2db9f12d77))

## [0.0.11](https://github.com/joangrigorov/rumil/compare/v0.0.10...v0.0.11) (2026-10-16)


### Bug Fixes

* advertise the tool schemas a provider validates ([#27](https://github.com/joangrigorov/rumil/issues/27)) ([0fa8188](https://github.com/joangrigorov/rumil/commit/0fa818871cf6f4eb0242f733b299fd61d78b53dc))

## [0.0.10](https://github.com/joangrigorov/rumil/compare/v0.0.9...v0.0.10) (2026-10-16)


### Features

* **config:** take the embedding width from the provider ([#25](https://github.com/joangrigorov/rumil/issues/25)) ([17ceb16](https://github.com/joangrigorov/rumil/commit/17ceb165a24f501de6fce25f31010d8e21465994))


### Bug Fixes

* **init:** ask a line at a time when the output is redirected ([#23](https://github.com/joangrigorov/rumil/issues/23)) ([cea6da9](https://github.com/joangrigorov/rumil/commit/cea6da958436105b2a79934856c475bb6c918a15))
* **init:** refuse the chat model as the embedding model ([#22](https://github.com/joangrigorov/rumil/issues/22)) ([5a72308](https://github.com/joangrigorov/rumil/commit/5a72308747514cc5b7a89692c603102e4450d8ad))

## [0.0.9](https://github.com/joangrigorov/rumil/compare/v0.0.8...v0.0.9) (2026-10-15)


### Features

* **config:** find the configuration file without --config ([#19](https://github.com/joangrigorov/rumil/issues/19)) ([b01024e](https://github.com/joangrigorov/rumil/commit/b01024e98acfb374c988f97e9de0bf700e65b536))
* **init:** write the configuration from a form ([#21](https://github.com/joangrigorov/rumil/issues/21)) ([4162426](https://github.com/joangrigorov/rumil/commit/41624269411ac039ee11e212706f20e986671783))

## [0.0.8](https://github.com/joangrigorov/rumil/compare/v0.0.7...v0.0.8) (2026-10-15)


### Features

* **license:** license rumil under Apache-2.0 and add a README ([#16](https://github.com/joangrigorov/rumil/issues/16)) ([46acedf](https://github.com/joangrigorov/rumil/commit/46acedf5378e47ebf47d9f2090cd2590ea894a41))

## [0.0.7](https://github.com/joangrigorov/rumil/compare/v0.0.6...v0.0.7) (2026-10-15)


### Features

* **memory:** let a maintainer promote a rule above the repository guidelines ([#14](https://github.com/joangrigorov/rumil/issues/14)) ([59a5811](https://github.com/joangrigorov/rumil/commit/59a5811ea0300fcf1fe2e40184a7c2a97d9a9df9))

## [0.0.6](https://github.com/joangrigorov/rumil/compare/v0.0.5...v0.0.6) (2026-10-15)


### Features

* **memory:** organization-wide rules, one hybrid query and moderation scope ([#11](https://github.com/joangrigorov/rumil/issues/11)) ([b84c7b9](https://github.com/joangrigorov/rumil/commit/b84c7b948425d03294ef95e6a58604d187e37d31))

## [0.0.5](https://github.com/joangrigorov/rumil/compare/v0.0.4...v0.0.5) (2026-10-14)


### Features

* **moderation:** the v0.0.5 gate surface, CLI and page ([#8](https://github.com/joangrigorov/rumil/issues/8)) ([2cd6de6](https://github.com/joangrigorov/rumil/commit/2cd6de69212095ae0b0e82093bfb6fe1ccf7029a))

## [0.0.4](https://github.com/joangrigorov/rumil/compare/v0.0.3...v0.0.4) (2026-10-14)


### Features

* **memory:** pgvector memory, the `@rumil` remember command, and re-review continuity ([#6](https://github.com/joangrigorov/rumil/issues/6)) ([2785bf0](https://github.com/joangrigorov/rumil/commit/2785bf0ac7563649edddc3231373fd037493c025))

## [0.0.3](https://github.com/joangrigorov/rumil/compare/v0.0.2...v0.0.3) (2026-10-14)


### Features

* **serve:** Phase 3 (v0.0.3) webhook server and review workers ([#4](https://github.com/joangrigorov/rumil/issues/4)) ([f8fa6fd](https://github.com/joangrigorov/rumil/commit/f8fa6fd18f54df9b59b3c8483e63e4e04b8cc9ce))

## [0.0.2](https://github.com/joangrigorov/rumil/compare/v0.0.1...v0.0.2) (2026-10-14)


### Features

* **reviewer:** Phase 2 (v0.0.2) deterministic RUMIL.md guidelines ([#3](https://github.com/joangrigorov/rumil/issues/3)) ([2adcac3](https://github.com/joangrigorov/rumil/commit/2adcac367c904f916306692e44ed23be19b04596))
* **review:** Phase 1 (v0.0.1) isolated runner and GitHub tool set ([#1](https://github.com/joangrigorov/rumil/issues/1)) ([ac3ab1c](https://github.com/joangrigorov/rumil/commit/ac3ab1c29b7220d3e375a7693b0a4313c3a69307))


### Documentation

* **plan:** record how a Release-As footer has to be delivered ([6c971a1](https://github.com/joangrigorov/rumil/commit/6c971a1892b2c25504a3d81116a11ccd999ac84d))
