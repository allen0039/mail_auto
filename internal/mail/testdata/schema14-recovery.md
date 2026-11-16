# Schema 14 recovery fixture

Source: `15c7160827d1ae56d3e177f818c02c450c53697e` (0.9.2), generated on 2026-11-16 by running the original `organizerFixture`, `executeFixture` with `uncertain=true`, and `NewPlan`, then exporting a consistent SQLite snapshot. The SQL preserves the exact schema 14 definitions of the eight relevant tables and the original five-field persisted plan digest; it does not derive that digest using the candidate's structs. Unrelated empty tables are omitted and created by startup migrations.

Only synthetic data is included. IDs are renamed, account ciphertext is removed (tests create their own temporary Vault ciphertext), irrelevant folder rows are omitted, and expiry is extended to keep the queued-plan test independent of the wall clock. The second unstarted plan is also represented as a persisted queued operation to exercise startup write rejection. No keys, tokens, private mail or live credentials are present.
