# Notes

**Rejected suggestion.** For the "one running sheet per account" rule, Claude
proposed a `HasRunningJob` store query that the start handler checks before
flipping the draft to running. I rejected it: two `POST /start` requests on two
drafts arriving at the same time would both pass the check and both end up
running, and the same window lets a double start of one draft overwrite the
first `runId`. Instead the rule lives in the database: a partial unique index
on `jobs (user_id) WHERE status = 'running'` (mapped to 409 in the handler) and
`StartJob` is a conditional `UPDATE ... WHERE status = 'draft' ... RETURNING`,
so the loser of a race sees no row and answers with whatever the winner wrote.

**How I checked it.** `TestStartRejectsASecondRunningSheet` and
`TestStartAgainReturnsTheSameRunID` cover the sequential cases against the
in-memory store. The race itself I checked against the real stack: with
`docker compose up`, two drafts with brochures, and two `curl ... /start` calls
fired in parallel, exactly one returned 200 and one 409, and Postgres showed a
single `running` row for the account.

**Also worth knowing.** The two tests the README names were rewritten and
renamed to describe the spec (`TestStartMarksTheJobSuccessful` ->
`TestStartMarksTheJobRunning`, `TestUploadKeepsTheClientFilename` ->
`TestUploadStoresTheBodyUnderUserAndJob`); each carries a comment with its old
name. `POST /start` on a `failed` or `success` sheet answers 409, since the
README only says a failed sheet stays failed and leaves the status code open.
The API moves brochures from the previous on-disk layout (`{DATA_DIR}/{jobID}/{name}`)
into place on start, so a `boarddata` volume written by the original code keeps
working. Uploads of one sheet are guarded the same way as starts: the bytes are
staged under a unique name, the sheet is claimed with a conditional `UPDATE ...
WHERE filename = ''`, and only the winner's file is moved into place.
