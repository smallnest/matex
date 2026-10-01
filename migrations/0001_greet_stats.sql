-- Example schema for the helloworld domain.
-- Kept portable across postgres / mysql / sqlite (VARCHAR for the key —
-- MySQL cannot index a bare TEXT column as a PRIMARY KEY).
CREATE TABLE IF NOT EXISTS greet_stats (
    name  VARCHAR(255) NOT NULL PRIMARY KEY,
    count INTEGER      NOT NULL DEFAULT 0
);
