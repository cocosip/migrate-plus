CREATE TABLE mp_child(id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL, CONSTRAINT mp_child_parent_fk FOREIGN KEY(parent_id) REFERENCES mp_items(id));
INSERT INTO mp_child(id,parent_id) VALUES(1,1);
