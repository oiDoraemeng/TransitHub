ALTER TABLE proxy_smart_groups
 ADD COLUMN usage_correction_enabled boolean NOT NULL DEFAULT false,
 ADD COLUMN usage_correction_allowance_percent integer NOT NULL DEFAULT 10
 CHECK (usage_correction_allowance_percent BETWEEN 0 AND 1000);
