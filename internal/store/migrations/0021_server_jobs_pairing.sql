-- BR-LINK-RULE: the pairing a claimed job belongs to (the Bridge's pairing
-- identity when the claim was recorded, a random id the app keeps in the
-- settings as pairing.id). An unpair on the Bridge's page clears it and a new
-- pairing makes a new one, so a start, or a change of pairing, drops the rows
-- of an earlier pairing (recorded lost, never resumed or reported) even when
-- the Bridge is paired with the same server again. Rows from before this
-- migration read '' and are given the pairing in place at the first start
-- (store.AdoptServerJobs).
ALTER TABLE server_jobs ADD COLUMN pairing TEXT NOT NULL DEFAULT '';
