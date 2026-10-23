CREATE TYPE order_status AS ENUM ('NEW', 'PROCESSING', 'INVALID', 'PROCESSED');

CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    login VARCHAR(256) NOT NULL UNIQUE,
    password_hash VARCHAR(60) NOT NULL,
    balance BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    withdrawn BIGINT NOT NULL DEFAULT 0 CHECK (withdrawn >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE orders (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    number VARCHAR(16384) NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id),
    status order_status NOT NULL DEFAULT 'NEW',
    accrual BIGINT CHECK (accrual >= 0),
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT orders_number_equal EXCLUDE USING hash (number WITH =) NOT DEFERRABLE
);

CREATE INDEX orders_user_time ON orders (user_id, uploaded_at DESC);
CREATE INDEX orders_pending ON orders (updated_at) WHERE status IN ('NEW', 'PROCESSING');

CREATE TABLE withdrawals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    order_number VARCHAR(16384) NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX withdrawals_user_time ON withdrawals (user_id, processed_at DESC);
