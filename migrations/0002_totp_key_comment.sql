-- Corrects the comment on user_totp, which named the wrong configuration key.
--
-- 0001 said the TOTP secret is sealed with
-- mail.dkim_private_key_encryption_key. That was true when TOTP was going to
-- borrow the DKIM key, and it stopped being true when auth.encryption_key was
-- introduced. They are unrelated keys: sharing one would mean rotating either
-- breaks the other, and a DKIM key is published in DNS while an encryption key
-- is not, so the two have different exposure requirements.
--
-- This is a new migration rather than an edit to 0001 on purpose. The runner
-- records a checksum per applied version and refuses to continue if a file
-- changes underneath it, which is the behaviour that stops a migration being
-- quietly rewritten after it has run somewhere. Rewriting 0001 to fix a comment
-- would break every database that has already applied it, including the
-- development one, over a change that carries no schema difference at all.

COMMENT ON TABLE user_totp IS
    'TOTP secrets are encrypted at rest with auth.encryption_key, which is '
    'deliberately not mail.dkim_private_key_encryption_key.';
