CREATE TABLE users (
    id BIGSERIAL PRIMARY KEY,
    login TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    withdrawn BIGINT NOT NULL DEFAULT 0 CHECK (withdrawn >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE orders (
    number TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    status TEXT NOT NULL DEFAULT 'NEW' CHECK (status IN ('NEW', 'PROCESSING', 'INVALID', 'PROCESSED')),
    accrual BIGINT CHECK (accrual >= 0),
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT orders_number_equal EXCLUDE USING hash (number WITH =) NOT DEFERRABLE
);

CREATE INDEX orders_user_time ON orders (user_id, uploaded_at DESC);
CREATE INDEX orders_pending ON orders (updated_at) WHERE status IN ('NEW', 'PROCESSING');

CREATE TABLE withdrawals (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    order_number TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX withdrawals_user_time ON withdrawals (user_id, processed_at DESC);
