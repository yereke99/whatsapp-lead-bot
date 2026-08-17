-- Replace the Airan campaign's trigger word with the full opt-in sentence.
--
-- The campaign used to be entered by sending "АЙРАН". A single common word is
-- a poor opt-in: it is what someone types when they are talking about ayran,
-- not when they are asking to join a webinar, and EXACT matching only narrows
-- that to messages which are nothing but the word. The sentence below is
-- unambiguous — nobody sends it by accident — and it is what the landing page
-- and ad creatives already tell people to write.
--
--   old:  АЙРАН
--   new:  Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді
--
-- Nothing else about the campaign changes: not its id, status, event time,
-- webinar link, steps, templates, template versions, enrolments, scheduled
-- messages or delivery history. This migration touches exactly one table,
-- campaign_triggers, and only the rows belonging to the campaign named "Airan".
--
-- Rollback (this project applies migrations forward only; run by hand if ever
-- needed, after stopping the service):
--
--   UPDATE campaign_triggers SET
--       keyword = 'АЙРАН', normalized_keyword = 'айран'
--   WHERE normalized_keyword = 'айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді'
--     AND campaign_id IN (SELECT id FROM campaigns WHERE lower(trim(name)) = 'airan');
--
--
-- On normalized_keyword
-- ---------------------
-- Matching compares textnorm.Normalize(message) against the stored
-- normalized_keyword, so the literal written here has to be byte-identical to
-- what the Go normalizer produces — case-folded, NFKC, whitespace collapsed. A
-- literal that drifted from the normalizer would leave a trigger that looks
-- correct in the panel and silently never fires, which is the worst possible
-- failure for an opt-in. TestMigratedTriggerLiteralMatchesNormalizer pins the
-- two together so a change to the normalizer breaks the build rather than the
-- funnel.
--
--
-- On a missing campaign
-- ---------------------
-- Doing nothing when there is no "Airan" campaign is deliberate, and it is not
-- the same as hiding a failure. Migrations run before the default campaign is
-- installed, so on every fresh database this statement legitimately finds
-- nothing to update; raising an error there would stop the service from
-- starting at all. Fresh installations get the new phrase from the seeder,
-- which carries the same sentence.


-- 1. Promote the existing trigger in place.
--
-- Updating rather than replacing keeps the trigger's id, which campaign_contacts
-- references through trigger_id: recreating the row would either break that
-- foreign key or rewrite the history of how every enrolled contact arrived.
-- Only the single oldest active row is promoted, so a database that somehow
-- holds several old-word triggers cannot collide on the unique index.
UPDATE campaign_triggers SET
    keyword            = 'Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді',
    normalized_keyword = 'айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді',
    match_mode         = 'EXACT',
    is_active          = 1
WHERE id = (
    SELECT t.id
    FROM campaign_triggers t
    JOIN campaigns c ON c.id = t.campaign_id
    WHERE lower(trim(c.name)) = 'airan'
      AND c.archived_at IS NULL
      AND t.normalized_keyword = 'айран'
      AND t.is_active
    ORDER BY t.created_at
    LIMIT 1
)
-- Skip when the campaign already carries the new phrase: re-running this
-- migration, or a database seeded after the change, must not be touched.
AND NOT EXISTS (
    SELECT 1
    FROM campaign_triggers t2
    JOIN campaigns c2 ON c2.id = t2.campaign_id
    WHERE lower(trim(c2.name)) = 'airan'
      AND c2.archived_at IS NULL
      AND t2.normalized_keyword = 'айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді'
);

-- 2. Create it when there was nothing to promote.
--
-- Covers an Airan campaign whose trigger was deleted by hand. If step 1 already
-- promoted a row, the NOT EXISTS below is false and this inserts nothing.
INSERT INTO campaign_triggers (campaign_id, keyword, normalized_keyword, match_mode, is_active)
SELECT
    c.id,
    'Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді',
    'айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді',
    'EXACT',
    1
FROM campaigns c
WHERE lower(trim(c.name)) = 'airan'
  AND c.archived_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM campaign_triggers t
    WHERE t.campaign_id = c.id
      AND t.normalized_keyword = 'айран/қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді'
  );

-- 3. Retire any old-word trigger still standing.
--
-- Reached when the new phrase already existed alongside the old one, so step 1
-- declined to promote. Deactivating rather than deleting keeps the row for the
-- enrolments that reference it, and matches how the panel retires a trigger.
-- Triggers on other campaigns are not touched: the word may legitimately belong
-- to somebody else's funnel.
UPDATE campaign_triggers SET is_active = 0
WHERE normalized_keyword = 'айран'
  AND is_active
  AND campaign_id IN (
    SELECT c.id FROM campaigns c
    WHERE lower(trim(c.name)) = 'airan'
      AND c.archived_at IS NULL
  );
