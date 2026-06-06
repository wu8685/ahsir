# AHSIR TODO

## Client Experience

- [ ] After a client sends chat to the scheduler and receives a response, support automatic voice playback on the client side.
  - Scope: client-side behavior after `ahsir chat` / scheduler chat response returns.
  - Design notes: keep scheduler response semantics unchanged; add an optional client feature flag/config so text output still works for non-audio environments.
  - Candidate implementation: macOS `say` first for local development, with an abstraction for other TTS backends later.
  - Requested: 2026-06-07.

