-- Phase 1d (BRIDGE-PLAN.md): chapterless books.
--
-- books.goal is how far a book runs: '' is the whole pipeline (characters, recaps,
-- description), 'chapters' stops after chapter_detecting (the chapters.detect job:
-- speech recognition and chapter detection only, no agent). ContinueChapters turns
-- a finished chapters book into a whole-pipeline one ('' again), so a later
-- sidecars.generate job reuses its transcription.
ALTER TABLE books ADD COLUMN goal TEXT NOT NULL DEFAULT '';
