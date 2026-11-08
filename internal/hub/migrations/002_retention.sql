ALTER TABLE hrx_hub_runs ADD COLUMN archived boolean NOT NULL DEFAULT false;
CREATE INDEX hrx_hub_runs_retention ON hrx_hub_runs(received_at)
    WHERE ended AND NOT archived;
