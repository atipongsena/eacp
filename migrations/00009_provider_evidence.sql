-- EACP Phase 9: provider evidence for AGT/ACS decisions (ADR-002 Rev 2.4 §8).
--
-- A decision from the Microsoft AGT sidecar records what the provider used to
-- decide: the ACS action identity, the matched rule, the Rego adapter digest
-- and the pinned AGT/ACS/OPA versions. The column is optional (the local
-- provider has none) and holds a JSON object only. Go bounds it to 4 KiB of
-- canonical JSON; PostgreSQL renders jsonb with spaces, so the database
-- backstop is 8 KiB of that rendering. Evidence stays immutable with its row.

-- +goose Up

ALTER TABLE eacp.decision_evidence
    ADD COLUMN provider_evidence jsonb
    CONSTRAINT decision_evidence_provider_evidence_object CHECK (
        provider_evidence IS NULL
        OR (jsonb_typeof(provider_evidence) = 'object' AND octet_length(provider_evidence::text) <= 8192));

-- +goose Down

ALTER TABLE eacp.decision_evidence DROP COLUMN provider_evidence;
