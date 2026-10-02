-- Full-text search indexes for IMAP SEARCH and webmail search.
-- Uses PostgreSQL tsvector/regexp for efficient text search across subject, from fields, and snippet.

ALTER TABLE messages ADD COLUMN IF NOT EXISTS search_vector tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', coalesce(subject, '')), 'A') ||
        setweight(to_tsvector('english', coalesce(from_name, '')), 'B') ||
        setweight(to_tsvector('english', coalesce(from_address, '')), 'B') ||
        setweight(to_tsvector('english', coalesce(snippet, '')), 'C')
    ) STORED;

CREATE INDEX IF NOT EXISTS messages_search_vector_idx ON messages USING gin(search_vector);
