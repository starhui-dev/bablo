-- +goose Up

-- Provider financial references identify one immutable liability globally.
-- Service-level advisory locks serialize creation; this constraint remains the
-- final defense against duplicate recovery facts from bugs or manual writes.
ALTER TABLE wallet_liabilities
    ADD CONSTRAINT wallet_liabilities_reference_uq
        UNIQUE (reference_type, reference_id);

-- +goose Down

ALTER TABLE wallet_liabilities
    DROP CONSTRAINT wallet_liabilities_reference_uq;
