-- 0047_run_terminal_size: a run's terminal size (ADR-0075). NULL in both columns is the node's default size
-- (runner.terminal_size): the node holds no attach for the run. A set size is held by the node through an interactive
-- attach of its own that never types; central sends the node the complete set of its runs' sizes (terminal.sizes,
-- node protocol minor 10). Both columns are set or both are NULL, within the bounds of runner.terminal_size.

-- +goose Up
ALTER TABLE runs ADD COLUMN terminal_cols INTEGER CHECK (terminal_cols IS NULL OR terminal_cols BETWEEN 40 AND 400);
ALTER TABLE runs ADD COLUMN terminal_rows INTEGER CHECK ((terminal_rows IS NULL) = (terminal_cols IS NULL) AND (terminal_rows IS NULL OR terminal_rows BETWEEN 10 AND 200));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
