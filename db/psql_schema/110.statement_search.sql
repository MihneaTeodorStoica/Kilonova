-- Keep accents in previews while matching Romanian and English words without accents.
-- The simple dictionary intentionally keeps stop words and does not guess a language.
CREATE TEXT SEARCH CONFIGURATION problem_search (COPY = pg_catalog.simple);
ALTER TEXT SEARCH CONFIGURATION problem_search
    ALTER MAPPING FOR hword, hword_part, word, asciiword, asciihword, hword_asciipart
    WITH unaccent, simple;

-- Markdown statements live in attachment bytes. Index them in bounded, overlapping
-- fragments so even very large statements do not exceed PostgreSQL's tsvector limit.
CREATE TABLE problem_statement_search (
    attachment_id bigint NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
    fragment integer NOT NULL,
    content text NOT NULL,
    search_vector tsvector NOT NULL,
    PRIMARY KEY (attachment_id, fragment)
);

CREATE INDEX problem_statement_search_vector_idx ON problem_statement_search USING gin (search_vector);

CREATE FUNCTION index_problem_statement(att_id bigint, att_name text, att_data bytea) RETURNS void AS $$
DECLARE
    body text;
    fragment_start integer;
    fragment_number integer := 0;
    fragment_text text;
BEGIN
    DELETE FROM problem_statement_search WHERE attachment_id = att_id;
    IF att_name !~ '^statement-[a-z]+(-[a-z]+)?[.]md$' THEN
        RETURN;
    END IF;

    -- Attachments can contain arbitrary bytes, even if they have an .md name.
    -- A malformed file must not prevent migrations or unrelated attachment edits.
    BEGIN
        body := convert_from(att_data, 'UTF8');
    EXCEPTION WHEN character_not_in_repertoire THEN
        RETURN;
    END;

    FOR fragment_start IN SELECT generate_series(1, length(body), 31744) LOOP
        fragment_text := substring(body FROM fragment_start FOR 32768);
        INSERT INTO problem_statement_search (attachment_id, fragment, content, search_vector)
        VALUES (att_id, fragment_number, fragment_text, to_tsvector('problem_search', fragment_text));
        fragment_number := fragment_number + 1;
    END LOOP;
END;
$$ LANGUAGE plpgsql;

CREATE FUNCTION refresh_problem_statement_search() RETURNS trigger AS $$
BEGIN
    PERFORM index_problem_statement(NEW.id, NEW.name, NEW.data);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER refresh_problem_statement_search
AFTER INSERT OR UPDATE OF name, data ON attachments
FOR EACH ROW EXECUTE FUNCTION refresh_problem_statement_search();

-- Backfill existing statements, including those already attached to problems.
DO $$
DECLARE att record;
BEGIN
    FOR att IN SELECT id, name, data FROM attachments WHERE name ~ '^statement-[a-z]+(-[a-z]+)?[.]md$' LOOP
        PERFORM index_problem_statement(att.id, att.name, att.data);
    END LOOP;
END;
$$;
