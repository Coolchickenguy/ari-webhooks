package enrich

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hackclub/ari-webhooks/internal/db"
	"github.com/hackclub/ari-webhooks/internal/filehours"
	"github.com/hackclub/ari-webhooks/internal/ids"
	"github.com/hackclub/ari-webhooks/internal/integrations/githost"
	"github.com/hackclub/ari-webhooks/internal/integrations/lapse"
	"github.com/hackclub/ari-webhooks/internal/secs"
)

type persistClip struct {
	clip    lapse.Clip
	makerId string
}

type persistInput struct {
	submissionId            string
	collab                  bool
	collaborators           []person
	commits                 []githost.Commit
	commitSeconds           []int
	htSeen                  func(githost.Commit) bool
	ownerOf                 func(githost.Commit) string
	trackFromMs             int64
	htHealthy               bool
	lapseWriteable          bool
	hackatimeSeconds        int
	afterLastCommitSeconds  int
	aiDiscountedSeconds     int
	hackatimeHeatmap        string // JSON: { "YYYY-MM-DD": { "<project>": seconds } }
	hackatimeHourly         string // JSON: { "YYYY-MM-DD": { "<5-min slot 0-287>": seconds } }
	hackatimeSecondsByMaker map[string]int
	afterLastSecondsByMaker map[string]int
	// Per-person project attribution as marshaled JSON ({ "<project>": seconds }
	// and its legacy minutes twin), keyed by makerId; missing maker = "{}"
	// (tracked, no time).
	projectSecondsJSONByMaker map[string]string
	projectMinutesJSONByMaker map[string]string
	clips                     []persistClip
	fullySynced               bool
	// treeRead gates anything stored from the file listing: an unreadable listing
	// must leave the previous snapshot alone rather than replacing it with nothing.
	treeRead bool
	files    []githost.File
	// fileHoursKnown gates the per-file hours write the same way: a capture that
	// never learned which files the time was spent in must leave the previous rows
	// alone rather than replacing them with nothing.
	fileHoursKnown bool
	fileHours      []filehours.FileHours
	// gathered is whatever the evidence seam collected before this write.
	gathered any
}

// persist ports enrich.ts stage 6: commits replaced atomically with degraded-HT
// carry-forward, pre-window journals dropped and re-settled, hours and clips
// written only when their source was healthy, and the processing→pending
// promotion guarded on fullySynced.
func (p *Pipeline) persist(ctx context.Context, in persistInput) ([]string, error) {
	var notes []string
	err := db.InTx(ctx, p.Pool, func(tx pgx.Tx) error {
		// Preserve evidence ids across recaptures so a held review's per-item
		// adjustments still address the same evidence when its values did not change.
		// codingSeconds + htSeen also carry forward on a degraded capture, and line
		// counts on a capture whose source could not count them.
		type priorCommit struct {
			id                   string
			codingSeconds        int
			htSeen               bool
			additions, deletions int
		}
		priorCommits := map[string]priorCommit{}
		if len(in.commits) > 0 {
			rows, err := tx.Query(ctx,
				`select id, hash, "codingSeconds", "htSeen", additions, deletions from "Commit" where "submissionId" = $1`, in.submissionId)
			if err != nil {
				return err
			}
			for rows.Next() {
				var hash string
				var prior priorCommit
				if err := rows.Scan(&prior.id, &hash, &prior.codingSeconds, &prior.htSeen, &prior.additions, &prior.deletions); err != nil {
					rows.Close()
					return err
				}
				priorCommits[hash] = prior
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `delete from "Commit" where "submissionId" = $1`, in.submissionId); err != nil {
			return err
		}
		for i, c := range in.commits {
			commitId := ids.Cuid()
			codingSeconds := 0
			htSeen := false
			prior, known := priorCommits[c.ShortHash]
			if known {
				commitId = prior.id
			}
			if in.htHealthy {
				codingSeconds = in.commitSeconds[i]
				htSeen = in.htSeen(c)
			} else if known {
				codingSeconds = prior.codingSeconds
				htSeen = prior.htSeen
			}
			var makerId any
			if in.collab {
				if owner := in.ownerOf(c); owner != "" {
					makerId = owner
				}
			}
			additions, deletions := c.Additions, c.Deletions
			if !c.LineStats && known {
				additions, deletions = prior.additions, prior.deletions // a clone cannot count lines: keep what an api read stored
			}
			coAuthors := c.CoAuthors
			if coAuthors == nil {
				coAuthors = []githost.CoAuthor{} // the column is NOT NULL with a [] default
			}
			if _, err := tx.Exec(ctx, `
				insert into "Commit" (id, "submissionId", hash, message, "committedAt", additions, deletions,
				                      "authorName", "authorEmail", "coAuthors", "makerId", "codingSeconds", "htSeen")
				values ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), nullif($9, ''), $10, $11, $12, $13)`,
				commitId, in.submissionId, c.ShortHash, c.Message, c.CommittedAt.UTC(), additions, deletions,
				c.AuthorName, c.AuthorEmail, coAuthors, makerId, codingSeconds, htSeen); err != nil {
				return err
			}
		}

		// The window start the rest of this capture was filtered against. Derived from
		// program/submission metadata alone, so it is written even on a degraded capture
		// where no hours land.
		var trackingFromAt any
		if in.trackFromMs > 0 {
			// 0 means nothing floors the window. It stays NULL rather than becoming the
			// epoch: the outbound justification renders this value as the start of the
			// range it reports, so a sentinel here tells the program the hours were
			// counted from 1/1/1970.
			trackingFromAt = time.UnixMilli(in.trackFromMs).UTC()
		}
		if _, err := tx.Exec(ctx, `
			insert into "HoursBreakdown" ("submissionId", "trackingFromAt")
			values ($1, $2)
			on conflict ("submissionId") do update set "trackingFromAt" = excluded."trackingFromAt"`,
			in.submissionId, trackingFromAt); err != nil {
			return err
		}

		if in.treeRead {
			fileNotes, err := p.Evidence.StoreFiles(ctx, tx, in.submissionId, in.files)
			if err != nil {
				return err
			}
			notes = append(notes, fileNotes...)
		}

		// Per-file hours are Hackatime-derived like codingSeconds: only a healthy
		// capture that actually resolved files may replace the snapshot.
		if in.htHealthy && in.fileHoursKnown {
			if _, err := tx.Exec(ctx,
				`delete from ariw."submissionFileHours" where "submissionId" = $1`, in.submissionId); err != nil {
				return err
			}
			fileWeights := make([]float64, len(in.fileHours))
			for i, fh := range in.fileHours {
				fileWeights[i] = fh.Seconds
			}
			_, fileSeconds := secs.Parts(fileWeights)
			for i, fh := range in.fileHours {
				var bytes any
				if fh.Status == "head" && fh.Bytes >= 0 { // a clone lists files without their sizes
					bytes = fh.Bytes
				}
				if _, err := tx.Exec(ctx, `
					insert into ariw."submissionFileHours" ("submissionId", path, seconds, bytes, status)
					values ($1, $2, $3, $4, $5)`,
					in.submissionId, fh.Path, fileSeconds[i], bytes, fh.Status); err != nil {
					return err
				}
			}
			notes = append(notes, fmt.Sprintf("file_hours_recorded_%d", len(in.fileHours)))
		}

		gatheredNotes, err := p.Evidence.Store(ctx, tx, in.submissionId, in.gathered)
		if err != nil {
			return err
		}
		notes = append(notes, gatheredNotes...)

		// Journal windowing: entries before the window start were already
		// credited; drop them and re-settle devlogMinutes. HT-independent, so it
		// runs even on a degraded capture.
		dropped, err := tx.Exec(ctx,
			`delete from "Devlog" where "submissionId" = $1 and at < $2`,
			in.submissionId, time.UnixMilli(in.trackFromMs).UTC())
		if err != nil {
			return err
		}
		if dropped.RowsAffected() > 0 {
			notes = append(notes, fmt.Sprintf("journals_before_window_%d", dropped.RowsAffected()))
			if _, err := tx.Exec(ctx, `
				insert into "HoursBreakdown" ("submissionId", "devlogSeconds", "devlogMinutes")
				select $1, coalesce(sum(seconds), 0), (coalesce(sum(seconds), 0) + 30) / 60
				from "Devlog" where "submissionId" = $1
				on conflict ("submissionId") do update
					set "devlogSeconds" = excluded."devlogSeconds", "devlogMinutes" = excluded."devlogMinutes"`,
				in.submissionId); err != nil {
				return err
			}
			if in.collab {
				for _, row := range in.collaborators {
					if _, err := tx.Exec(ctx, `
						update "SubmissionCollaborator" set ("devlogSeconds", "devlogMinutes") = (
							select coalesce(sum(seconds), 0), (coalesce(sum(seconds), 0) + 30) / 60 from "Devlog"
							where "submissionId" = $2 and "makerId" = $3
						) where id = $1`,
						row.collaboratorRow, in.submissionId, row.makerId); err != nil {
						return err
					}
				}
			}
		}

		if in.htHealthy {
			if _, err := tx.Exec(ctx, `
				insert into "HoursBreakdown" ("submissionId", "hackatimeSeconds", "afterLastCommitSeconds", "aiDiscountedSeconds",
				                              "hackatimeMinutes", "devlogMinutes", "afterLastCommitMinutes", "aiDiscountedMinutes",
				                              "hackatimeHeatmap", "hackatimeHourly")
				values ($1, $2, $3, $4, $5, 0, $6, $7, $8::jsonb, $9::jsonb)
				on conflict ("submissionId") do update
					set "hackatimeSeconds" = excluded."hackatimeSeconds",
					    "afterLastCommitSeconds" = excluded."afterLastCommitSeconds",
					    "aiDiscountedSeconds" = excluded."aiDiscountedSeconds",
					    "hackatimeMinutes" = excluded."hackatimeMinutes",
					    "afterLastCommitMinutes" = excluded."afterLastCommitMinutes",
					    "aiDiscountedMinutes" = excluded."aiDiscountedMinutes",
					    "hackatimeHeatmap" = excluded."hackatimeHeatmap",
					    "hackatimeHourly" = excluded."hackatimeHourly"`,
				in.submissionId, in.hackatimeSeconds, in.afterLastCommitSeconds, in.aiDiscountedSeconds,
				secs.LegacyMinutes(in.hackatimeSeconds), secs.LegacyMinutes(in.afterLastCommitSeconds),
				secs.LegacyMinutes(in.aiDiscountedSeconds), in.hackatimeHeatmap, in.hackatimeHourly); err != nil {
				return err
			}
			if in.collab {
				for _, row := range in.collaborators {
					projectSeconds := in.projectSecondsJSONByMaker[row.makerId]
					projectMinutes := in.projectMinutesJSONByMaker[row.makerId]
					if projectSeconds == "" {
						projectSeconds, projectMinutes = "{}", "{}"
					}
					hackatimeSeconds := in.hackatimeSecondsByMaker[row.makerId]
					afterLastSeconds := in.afterLastSecondsByMaker[row.makerId]
					if _, err := tx.Exec(ctx, `
						update "SubmissionCollaborator"
						set "hackatimeSeconds" = $2, "afterLastCommitSeconds" = $3, "hackatimeProjectSeconds" = $4::jsonb,
						    "hackatimeMinutes" = $5, "afterLastCommitMinutes" = $6, "hackatimeProjectMinutes" = $7::jsonb
						where id = $1`,
						row.collaboratorRow, hackatimeSeconds, afterLastSeconds, projectSeconds,
						secs.LegacyMinutes(hackatimeSeconds), secs.LegacyMinutes(afterLastSeconds),
						projectMinutes); err != nil {
						return err
					}
				}
			}
		}

		if in.lapseWriteable {
			priorClips := map[string]string{}
			rows, err := tx.Query(ctx, `
				select id, at, "lengthSeconds", note, coalesce(url, ''), coalesce("makerId", '')
				from "ElapsedClip" where "submissionId" = $1`, in.submissionId)
			if err != nil {
				return err
			}
			for rows.Next() {
				var id, note, url, makerId string
				var at time.Time
				var lengthSeconds int
				if err := rows.Scan(&id, &at, &lengthSeconds, &note, &url, &makerId); err != nil {
					rows.Close()
					return err
				}
				priorClips[clipKey(at, lengthSeconds, note, url, makerId)] = id
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			clipWeights := make([]float64, len(in.clips))
			for i, c := range in.clips {
				clipWeights[i] = c.clip.DurationSeconds
			}
			lapseSeconds, clipSeconds := secs.Parts(clipWeights)
			if _, err := tx.Exec(ctx, `
				insert into "HoursBreakdown" ("submissionId", "lapseSeconds", "lapseMinutes")
				values ($1, $2, $3)
				on conflict ("submissionId") do update
					set "lapseSeconds" = excluded."lapseSeconds", "lapseMinutes" = excluded."lapseMinutes"`,
				in.submissionId, lapseSeconds, secs.LegacyMinutes(lapseSeconds)); err != nil {
				return err
			}
			if in.collab {
				for _, row := range in.collaborators {
					ownSeconds := 0
					for i, c := range in.clips {
						if c.makerId == row.makerId {
							ownSeconds += clipSeconds[i]
						}
					}
					if _, err := tx.Exec(ctx, `
						update "SubmissionCollaborator" set "lapseSeconds" = $2, "lapseMinutes" = $3 where id = $1`,
						row.collaboratorRow, ownSeconds, secs.LegacyMinutes(ownSeconds)); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec(ctx, `delete from "ElapsedClip" where "submissionId" = $1`, in.submissionId); err != nil {
				return err
			}
			for i, c := range in.clips {
				makerId := ""
				if in.collab {
					makerId = c.makerId
				}
				lengthSeconds := clipSeconds[i]
				key := clipKey(c.clip.CreatedAt, lengthSeconds, c.clip.Name, c.clip.PlaybackUrl, makerId)
				clipId := priorClips[key]
				if clipId == "" {
					clipId = ids.Cuid()
				} else {
					delete(priorClips, key)
				}
				var storedMakerId any
				if makerId != "" {
					storedMakerId = makerId
				}
				if _, err := tx.Exec(ctx, `
					insert into "ElapsedClip" (id, "submissionId", at, "lengthSeconds", note, url, "thumbnailUrl", "makerId")
					values ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8)`,
					clipId, in.submissionId, c.clip.CreatedAt.UTC(), lengthSeconds,
					c.clip.Name, c.clip.PlaybackUrl, c.clip.ThumbnailUrl, storedMakerId); err != nil {
					return err
				}
			}
		}

		// Every persisted capture is a new evidence snapshot, degraded or not - the
		// counter lets ari (and the review screen's resync poll) tell one from the next.
		if _, err := tx.Exec(ctx,
			`update "Submission" set "enrichmentVersion" = "enrichmentVersion" + 1 where id = $1`,
			in.submissionId); err != nil {
			return err
		}

		if in.fullySynced {
			if _, err := tx.Exec(ctx,
				`update "Submission" set "evidenceSyncedAt" = now() where id = $1`, in.submissionId); err != nil {
				return err
			}
			// Enrichment complete: the ship becomes visible in the review queue.
			if _, err := tx.Exec(ctx, `
				update "Submission" set status = 'pending' where id = $1 and status = 'processing'`,
				in.submissionId); err != nil {
				return err
			}
		}
		return nil
	})
	return notes, err
}

func clipKey(at time.Time, lengthSeconds int, note, url, makerId string) string {
	return fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s", at.UTC().UnixNano(), lengthSeconds, note, url, makerId)
}
