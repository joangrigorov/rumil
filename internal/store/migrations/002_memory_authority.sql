-- +goose Up
-- A rule's authority over the repository's own guidelines. CONTEXT is what
-- every rule stored before this migration keeps: recorded context that the
-- guidelines outrank. DIRECTIVE is set only at the approval gate, by a
-- maintainer, and puts one rule above RUMIL.md for the case it names.
--
-- This is the first migration after the initial schema, and it is additive: the
-- column is added with a non-volatile default, so an installation that already
-- holds rules self-migrates on boot and every stored row reads back as CONTEXT,
-- which is how each of those rules behaved before the column existed. A
-- destructive migration is still never written; §4 of project_plan.md carries
-- that rule.
CREATE TYPE memory_authority AS ENUM ('CONTEXT', 'DIRECTIVE');

ALTER TABLE memories
    ADD COLUMN authority memory_authority NOT NULL DEFAULT 'CONTEXT';

-- A promotion reaches one repository, so the pair is refused at rest as well as
-- by the statement that writes it: a reader can rely on a DIRECTIVE rule being a
-- repository's own, whatever wrote the row. Every row this migration meets is
-- CONTEXT, so the constraint holds for all of them.
ALTER TABLE memories
    ADD CONSTRAINT memories_directive_is_a_repository_rule
        CHECK (authority <> 'DIRECTIVE' OR scope = 'REPO');

-- +goose Down
ALTER TABLE memories DROP CONSTRAINT memories_directive_is_a_repository_rule;
ALTER TABLE memories DROP COLUMN authority;
DROP TYPE IF EXISTS memory_authority;
