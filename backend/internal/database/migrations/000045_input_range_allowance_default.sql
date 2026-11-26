-- Existing groups keep their chosen allowance. Only new/default values change.
ALTER TABLE proxy_smart_groups ALTER COLUMN usage_correction_allowance_percent SET DEFAULT 40;
