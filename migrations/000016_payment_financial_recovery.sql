-- +goose Up

ALTER TABLE wallets
    ADD COLUMN financial_hold boolean NOT NULL DEFAULT false;

UPDATE wallets wallet
SET financial_hold = true
WHERE EXISTS (
    SELECT 1
    FROM wallet_reservations reservation
    JOIN billing_settlements settlement ON settlement.reservation_id = reservation.id
    WHERE reservation.wallet_id = wallet.id AND settlement.status = 'pending'
);

ALTER TABLE billing_settlements
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN owner_token uuid,
    ADD COLUMN lease_expires_at timestamptz,
    ADD CONSTRAINT billing_settlements_recovery_lease_check CHECK (
        (owner_token IS NULL AND lease_expires_at IS NULL)
        OR (owner_token IS NOT NULL AND lease_expires_at IS NOT NULL)
    );

CREATE INDEX billing_settlements_recovery_idx
    ON billing_settlements (next_attempt_at, id)
    WHERE status = 'pending';


ALTER TABLE wallet_ledger
    DROP CONSTRAINT wallet_ledger_entry_type_check,
    DROP CONSTRAINT wallet_ledger_amount_semantics_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'admin_adjustment', 'grant', 'bonus', 'expiration',
            'payment_refund_hold', 'payment_reversal', 'payment_refund_release',
            'payment_liability'
        )),
    ADD CONSTRAINT wallet_ledger_amount_semantics_check
        CHECK (
            (entry_type = 'reservation'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'usage_charge'
                AND amount_minor < 0
                AND available_delta_minor <= 0
                AND reserved_delta_minor <= 0
                AND available_delta_minor + reserved_delta_minor = amount_minor)
            OR (entry_type IN ('recharge', 'refund', 'grant', 'bonus')
                AND amount_minor > 0
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
            OR (entry_type IN ('adjustment', 'admin_adjustment', 'expiration', 'payment_liability')
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
            OR (entry_type = 'payment_refund_hold'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_reversal'
                AND amount_minor < 0
                AND available_delta_minor = 0
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_refund_release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
        );

CREATE TABLE wallet_liabilities (
    id uuid PRIMARY KEY,
    wallet_id uuid NOT NULL REFERENCES wallets(id),
    liability_type text NOT NULL CHECK (liability_type IN ('payment_refund', 'payment_dispute')),
    reference_type text NOT NULL,
    reference_id text NOT NULL,
    principal_amount_minor bigint NOT NULL CHECK (principal_amount_minor > 0),
    recovered_amount_minor bigint NOT NULL DEFAULT 0 CHECK (recovered_amount_minor >= 0),
    currency char(3) NOT NULL,
    status text NOT NULL CHECK (status IN ('open', 'settled', 'reversed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'open' AND recovered_amount_minor < principal_amount_minor)
        OR (status = 'settled' AND recovered_amount_minor = principal_amount_minor)
        OR status = 'reversed'),
    CHECK (recovered_amount_minor <= principal_amount_minor),
    CHECK (currency = upper(currency) AND currency ~ '^[A-Z]{3}$'),
    CHECK (reference_type = btrim(reference_type) AND length(reference_type) BETWEEN 1 AND 64),
    CHECK (reference_id = btrim(reference_id) AND length(reference_id) BETWEEN 1 AND 160)
);
CREATE INDEX wallet_liabilities_wallet_status_idx
    ON wallet_liabilities (wallet_id, status, created_at, id);

-- +goose StatementBegin
CREATE FUNCTION bablo_validate_wallet_liability_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallet liabilities cannot be deleted' USING ERRCODE = '55000';
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.wallet_id <> OLD.wallet_id
        OR NEW.liability_type <> OLD.liability_type
        OR NEW.reference_type <> OLD.reference_type
        OR NEW.reference_id <> OLD.reference_id
        OR NEW.principal_amount_minor <> OLD.principal_amount_minor
        OR NEW.currency <> OLD.currency
        OR NEW.created_at <> OLD.created_at
        OR NEW.recovered_amount_minor < OLD.recovered_amount_minor
        OR OLD.status = 'reversed'
        OR (OLD.status = 'settled' AND NEW.status <> 'reversed')
    THEN
        RAISE EXCEPTION 'invalid wallet liability mutation' USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_liabilities_mutation_guard
    BEFORE UPDATE OR DELETE ON wallet_liabilities
    FOR EACH ROW EXECUTE FUNCTION bablo_validate_wallet_liability_mutation();
CREATE TABLE payment_funding_operations (
    idempotency_key text PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    currency char(3) NOT NULL,
    amount_minor bigint NOT NULL CHECK (amount_minor > 0),
    operator_user_id uuid NOT NULL REFERENCES users(id),
    ledger_id uuid NOT NULL UNIQUE REFERENCES wallet_ledger(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (idempotency_key = btrim(idempotency_key) AND length(idempotency_key) BETWEEN 1 AND 160),
    CHECK (currency = upper(currency) AND currency ~ '^[A-Z]{3}$')
);

-- +goose StatementBegin
CREATE FUNCTION bablo_reject_payment_funding_operation_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'payment funding operations are immutable'
        USING ERRCODE = '55000';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payment_funding_operations_immutable
    BEFORE UPDATE OR DELETE ON payment_funding_operations
    FOR EACH ROW EXECUTE FUNCTION bablo_reject_payment_funding_operation_mutation();

-- +goose Down

DROP TRIGGER payment_funding_operations_immutable ON payment_funding_operations;
DROP FUNCTION bablo_reject_payment_funding_operation_mutation();
DROP TABLE payment_funding_operations;

DROP TRIGGER wallet_liabilities_mutation_guard ON wallet_liabilities;
DROP FUNCTION bablo_validate_wallet_liability_mutation();
DROP TABLE wallet_liabilities;

ALTER TABLE wallet_ledger
    DROP CONSTRAINT wallet_ledger_entry_type_check,
    DROP CONSTRAINT wallet_ledger_amount_semantics_check;
ALTER TABLE wallet_ledger
    ADD CONSTRAINT wallet_ledger_entry_type_check
        CHECK (entry_type IN (
            'reservation', 'usage_charge', 'release', 'recharge', 'refund',
            'adjustment', 'admin_adjustment', 'grant', 'bonus', 'expiration',
            'payment_refund_hold', 'payment_reversal', 'payment_refund_release'
        )),
    ADD CONSTRAINT wallet_ledger_amount_semantics_check
        CHECK (
            (entry_type = 'reservation'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'usage_charge'
                AND amount_minor < 0
                AND available_delta_minor <= 0
                AND reserved_delta_minor <= 0
                AND available_delta_minor + reserved_delta_minor = amount_minor)
            OR (entry_type IN ('recharge', 'refund', 'grant', 'bonus')
                AND amount_minor > 0
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
            OR (entry_type IN ('adjustment', 'admin_adjustment', 'expiration')
                AND available_delta_minor = amount_minor
                AND reserved_delta_minor = 0)
            OR (entry_type = 'payment_refund_hold'
                AND amount_minor > 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_reversal'
                AND amount_minor < 0
                AND available_delta_minor = 0
                AND reserved_delta_minor = amount_minor)
            OR (entry_type = 'payment_refund_release'
                AND amount_minor < 0
                AND available_delta_minor = -amount_minor
                AND reserved_delta_minor = amount_minor)
        );

DROP INDEX billing_settlements_recovery_idx;
ALTER TABLE billing_settlements
    DROP CONSTRAINT billing_settlements_recovery_lease_check,
    DROP COLUMN lease_expires_at,
    DROP COLUMN owner_token,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_at;
ALTER TABLE wallets DROP COLUMN financial_hold;
