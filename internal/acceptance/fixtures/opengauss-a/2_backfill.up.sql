ALTER TABLE mp_items ADD note VARCHAR(200) DEFAULT 'backfill';
UPDATE mp_items SET note='backfill' WHERE id=1;
