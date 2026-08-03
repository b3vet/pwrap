-- RLS example: restrict pwrap_documents so each row is visible only to the user
-- whose id matches `data->>'user_id'`. The same policy applies whether the query
-- reaches the DB through the pwrap SDK (via Client.WithUser) or through PostgREST
-- (via a JWT claim) — because both paths put the user id in request.jwt.claims.
--
-- FORCE makes the policy apply to the table owner too; without it, the tenant role
-- (which owns pwrap_documents) would bypass RLS and see everything.

ALTER TABLE pwrap_documents ENABLE  ROW LEVEL SECURITY;
ALTER TABLE pwrap_documents FORCE   ROW LEVEL SECURITY;

DROP POLICY IF EXISTS pwrap_documents_user_policy ON pwrap_documents;

-- NULLIF guards against the "no claims set" case. current_setting(..., true) returns
-- an empty string when the GUC hasn't been set, and casting '' to jsonb fails with
-- 22P02 — turning the setting into NULL first makes the policy cleanly deny instead.
CREATE POLICY pwrap_documents_user_policy ON pwrap_documents
    USING      (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
    WITH CHECK (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
