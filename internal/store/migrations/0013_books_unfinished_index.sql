-- A partial index over the unfinished books (bridge#31): every book but a
-- finished one (state done with no status), the predicate the periodic passes
-- read (store.unfinishedBook: the 5 s dispatch, the 30 s supervisor tick and
-- the stage-run reads they make). Finished books outnumber the rest on a long
-- running Bridge, so those reads scan only the rows the engine may still act on.
-- The WHERE must stay word for word the predicate the queries use: SQLite takes
-- a partial index only when the query's WHERE implies the index's.
CREATE INDEX idx_books_unfinished ON books(id) WHERE NOT (state = 'done' AND status = '');
