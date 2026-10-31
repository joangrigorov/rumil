# Changelog

## [0.0.26](https://github.com/joangrigorov/rumil/compare/v0.0.25...v0.0.26) (2026-10-31)


### Features

* **config:** hot-reload the active configuration file ([#108](https://github.com/joangrigorov/rumil/issues/108)) ([f1dab03](https://github.com/joangrigorov/rumil/commit/f1dab03cf0cd6e82f52e7fa4dbf8913593a8f950))

## [0.0.25](https://github.com/joangrigorov/rumil/compare/v0.0.24...v0.0.25) (2026-10-30)


### Features

* **deck:** add thinking toggle, markdown output, and session pagination ([#106](https://github.com/joangrigorov/rumil/issues/106)) ([fac30cc](https://github.com/joangrigorov/rumil/commit/fac30cc1a578f19ac540632b7d6cae4e366ad693))

## [0.0.24](https://github.com/joangrigorov/rumil/compare/v0.0.23...v0.0.24) (2026-10-30)


### Features

* **reviewer:** show a pull request's RUMIL.md as a proposal ([#104](https://github.com/joangrigorov/rumil/issues/104)) ([852a29e](https://github.com/joangrigorov/rumil/commit/852a29efe4daf757f096053a9842ee7d936b08f7))

## [0.0.23](https://github.com/joangrigorov/rumil/compare/v0.0.22...v0.0.23) (2026-10-30)


### Bug Fixes

* **build:** build the from-source image with Go 1.27.3 ([#102](https://github.com/joangrigorov/rumil/issues/102)) ([4fc7828](https://github.com/joangrigorov/rumil/commit/4fc7828fa38e7065b07b6eea58dfba63ba44e929))

## [0.0.22](https://github.com/joangrigorov/rumil/compare/v0.0.21...v0.0.22) (2026-10-30)


### Features

* **reviewer:** focus the answer on the question after why ([#98](https://github.com/joangrigorov/rumil/issues/98)) ([aad1172](https://github.com/joangrigorov/rumil/commit/aad1172d189f12808f7916363fc2b15b2d545ab7))

## [0.0.21](https://github.com/joangrigorov/rumil/compare/v0.0.20...v0.0.21) (2026-10-30)


### Features

* **doctor:** check the configuration against every dependency ([#92](https://github.com/joangrigorov/rumil/issues/92)) ([9769041](https://github.com/joangrigorov/rumil/commit/9769041ba720bdecbaba4df40432302e2a40aedb))
* **reviewer:** answer questions in a review thread ([#88](https://github.com/joangrigorov/rumil/issues/88)) ([8189b4d](https://github.com/joangrigorov/rumil/commit/8189b4d6301f5c7aceb4f6dbfd55b5b3a065ecf1))
* **reviewer:** fall back to a secondary chat provider ([#91](https://github.com/joangrigorov/rumil/issues/91)) ([573f2d7](https://github.com/joangrigorov/rumil/commit/573f2d73a283c7581227e2b030fbff0e8f251a73))
* **reviewer:** read per-repository review settings from the base branch ([#90](https://github.com/joangrigorov/rumil/issues/90)) ([bb08ea8](https://github.com/joangrigorov/rumil/commit/bb08ea846e638a867772ca6ce6ed8e13fda01ce1))
* **server:** expose Prometheus metrics ([#93](https://github.com/joangrigorov/rumil/issues/93)) ([99c7601](https://github.com/joangrigorov/rumil/commit/99c76015dd2c88e1d2644c0c170ae77e49e06d4f))
* **server:** name admin credentials and record who used them ([#94](https://github.com/joangrigorov/rumil/issues/94)) ([b9a757f](https://github.com/joangrigorov/rumil/commit/b9a757fc53dc4ddb7b9b04b2cbbf020a174a9430))
* **server:** serve several App installations in one process ([#96](https://github.com/joangrigorov/rumil/issues/96)) ([91d89ba](https://github.com/joangrigorov/rumil/commit/91d89ba6ef3560b7bb8826a0655b5b65e1a30a30)), closes [#79](https://github.com/joangrigorov/rumil/issues/79)


### Bug Fixes

* **doctor:** accept named admin credentials ([#95](https://github.com/joangrigorov/rumil/issues/95)) ([0ac78ff](https://github.com/joangrigorov/rumil/commit/0ac78ff58e57d84699507387caaf651a7edf394f)), closes [#77](https://github.com/joangrigorov/rumil/issues/77)

## [0.0.20](https://github.com/joangrigorov/rumil/compare/v0.0.19...v0.0.20) (2026-10-30)


### Features

* **eval:** run the reviewer against a fixture set and score the findings ([#87](https://github.com/joangrigorov/rumil/issues/87)) ([777ba50](https://github.com/joangrigorov/rumil/commit/777ba5010c5a05ab16e66789b551729fd980c6b6)), closes [#72](https://github.com/joangrigorov/rumil/issues/72)
* **reviewer:** let a review call tools from an operator MCP server ([#84](https://github.com/joangrigorov/rumil/issues/84)) ([05d3d7d](https://github.com/joangrigorov/rumil/commit/05d3d7d5da8045d1a05408d572fd1003315c9e61)), closes [#69](https://github.com/joangrigorov/rumil/issues/69)
* **reviewer:** record token usage and estimated cost with a budget ([#85](https://github.com/joangrigorov/rumil/issues/85)) ([526f480](https://github.com/joangrigorov/rumil/commit/526f48075775b88e5c64f53d117a0c008a306f79)), closes [#70](https://github.com/joangrigorov/rumil/issues/70)
* **reviewer:** report each review as a GitHub check run ([#82](https://github.com/joangrigorov/rumil/issues/82)) ([07ed516](https://github.com/joangrigorov/rumil/commit/07ed5161aca7ad2f5c3a0e53551e1bc19694943d))
* **server:** show the queue in the deck and let an operator cancel a review ([#86](https://github.com/joangrigorov/rumil/issues/86)) ([428ca0d](https://github.com/joangrigorov/rumil/commit/428ca0d1d8d0cde9d0be9d79b7efb39d7853049e)), closes [#78](https://github.com/joangrigorov/rumil/issues/78)
* **store:** record the outcome of every finding Rumil posts ([#83](https://github.com/joangrigorov/rumil/issues/83)) ([24a06b9](https://github.com/joangrigorov/rumil/commit/24a06b94b0206640c8ef23dfc6e48d44f0fa1c70))


### Bug Fixes

* **reviewer:** treat pull request content as untrusted in the prompt ([#80](https://github.com/joangrigorov/rumil/issues/80)) ([a46ca27](https://github.com/joangrigorov/rumil/commit/a46ca2765625e3d12f9a234b9ddec2e0b6b34e74))

## [0.0.19](https://github.com/joangrigorov/rumil/compare/v0.0.18...v0.0.19) (2026-10-30)


### Features

* **server:** carry a review command's focus into the task message ([#66](https://github.com/joangrigorov/rumil/issues/66)) ([5960319](https://github.com/joangrigorov/rumil/commit/596031910eec3fff5ee5fd20602ed7184f5c147f))


### Bug Fixes

* **ghapp:** read a large pull request's diff from the files API ([#60](https://github.com/joangrigorov/rumil/issues/60)) ([a5bd299](https://github.com/joangrigorov/rumil/commit/a5bd299e3636d224c7249e522f37e16d35d48c73))
* **models:** bound the wait for a provider's response headers ([#63](https://github.com/joangrigorov/rumil/issues/63)) ([9e70b5e](https://github.com/joangrigorov/rumil/commit/9e70b5ee3315b3fa99fdad6f71afeeaf64fa3699))
* **reviewer:** ask the model to continue when a turn ends without a review ([#62](https://github.com/joangrigorov/rumil/issues/62)) ([26bcc4c](https://github.com/joangrigorov/rumil/commit/26bcc4cfc8893d0633ddd8a7b04b7f9b31f9b2c5))
* **reviewer:** name the provider failure in a no-review error ([#64](https://github.com/joangrigorov/rumil/issues/64)) ([cdf1dcb](https://github.com/joangrigorov/rumil/commit/cdf1dcba56094c5806b59213eef690fc6096ada5))
* **server:** let the session stream authenticate with a read cookie ([#65](https://github.com/joangrigorov/rumil/issues/65)) ([3b1e01b](https://github.com/joangrigorov/rumil/commit/3b1e01b60e279e1a1d9676a220a98ccf767d7635))

## [0.0.18](https://github.com/joangrigorov/rumil/compare/v0.0.17...v0.0.18) (2026-10-30)


### Features

* **config:** let an embedding provider set its request timeout ([#58](https://github.com/joangrigorov/rumil/issues/58)) ([cebfa93](https://github.com/joangrigorov/rumil/commit/cebfa93fc7eaa221f42950cfa806b87f73f6a005))

## [0.0.17](https://github.com/joangrigorov/rumil/compare/v0.0.16...v0.0.17) (2026-10-29)


### Features

* **reviewer:** give the reviewer a local working tree to read ([#56](https://github.com/joangrigorov/rumil/issues/56)) ([7c3780e](https://github.com/joangrigorov/rumil/commit/7c3780e801946e991ad22ff7cc6fde91f35ae286))

## [0.0.16](https://github.com/joangrigorov/rumil/compare/v0.0.15...v0.0.16) (2026-10-29)


### Bug Fixes

* **reviewer:** recover from a response cut off at the output limit ([#54](https://github.com/joangrigorov/rumil/issues/54)) ([4f52c88](https://github.com/joangrigorov/rumil/commit/4f52c8893a131ad685cfc8a40436d4a115cdf80e))

## [0.0.15](https://github.com/joangrigorov/rumil/compare/v0.0.14...v0.0.15) (2026-10-29)


### Features

* **compaction:** add configurable context compaction ([#46](https://github.com/joangrigorov/rumil/issues/46)) ([d4b9063](https://github.com/joangrigorov/rumil/commit/d4b9063c10c58efd0e09fc83ca7e9645e367076c))
* **config:** allow unlimited agent turns ([#47](https://github.com/joangrigorov/rumil/issues/47)) ([555fe77](https://github.com/joangrigorov/rumil/commit/555fe772b39f4447931eabf10040f0dfeba57670))
* **models:** add a reasoning level per provider ([#50](https://github.com/joangrigorov/rumil/issues/50)) ([27668d6](https://github.com/joangrigorov/rumil/commit/27668d61b26b326628c080651dd82b89b110aaaa))
* **reviewer:** continue a review after the turn limit ([#53](https://github.com/joangrigorov/rumil/issues/53)) ([aae1839](https://github.com/joangrigorov/rumil/commit/aae1839a3b1a8cfec3ac007c1b359c5c63e6ab80))
* **worker:** make the job timeout configurable ([#51](https://github.com/joangrigorov/rumil/issues/51)) ([4126cbd](https://github.com/joangrigorov/rumil/commit/4126cbd968886c27d90ada7b073524dc7c5b510e))


### Bug Fixes

* **deps:** bump the github-actions-dependencies group across 1 directory with 2 updates ([#43](https://github.com/joangrigorov/rumil/issues/43)) ([6cfef0f](https://github.com/joangrigorov/rumil/commit/6cfef0f02ead682aa83579bcad2423322ebddb24))
* **deps:** bump the go-dependencies group across 1 directory with 2 updates ([#44](https://github.com/joangrigorov/rumil/issues/44)) ([918ad72](https://github.com/joangrigorov/rumil/commit/918ad722eeaf2733f4781dbc7aa1983b4e48ad7c))
* **review:** fail a session that submits no review ([#49](https://github.com/joangrigorov/rumil/issues/49)) ([c3e0751](https://github.com/joangrigorov/rumil/commit/c3e07515acf964ea96bcbfb8bd212f5b37ea31c9))

## [0.0.14](https://github.com/joangrigorov/rumil/compare/v0.0.13...v0.0.14) (2026-10-17)


### Features

* add the review session observation deck ([#40](https://github.com/joangrigorov/rumil/issues/40)) ([3f708fc](https://github.com/joangrigorov/rumil/commit/3f708fc4f6dddcb60135633e42442e6097280032))
* give the moderation page a real design ([#38](https://github.com/joangrigorov/rumil/issues/38)) ([8ee6dc0](https://github.com/joangrigorov/rumil/commit/8ee6dc01aa86d108f06877f5c51f1a442d6d525e))

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
