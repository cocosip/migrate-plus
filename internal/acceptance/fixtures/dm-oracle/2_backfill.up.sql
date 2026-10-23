ALTER TABLE mp_items ADD note VARCHAR2(200) DEFAULT 'backfill';
UPDATE mp_items SET note='backfill' WHERE id=1;
