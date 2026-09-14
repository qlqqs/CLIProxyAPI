package service

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func (c *Control) PreviewRetention(ctx context.Context, actor domain.User, operation string) (domain.RetentionJob, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.RetentionJob{}, ErrForbidden
	}
	if !validRetentionOperation(operation) {
		return domain.RetentionJob{}, domain.ErrInvalid
	}
	cutoff := c.currentTime()
	if operation == "usage_details" {
		settings, err := c.RetentionSettings(ctx, actor)
		if err != nil {
			return domain.RetentionJob{}, err
		}
		if settings.EffectiveDays == 0 {
			// SQLite timestamps have microsecond precision. Include completed records
			// at the current instant for an explicit cleanup under a permanent policy.
			cutoff = cutoff.Add(time.Microsecond)
		} else {
			cutoff = cutoff.Add(-time.Duration(settings.EffectiveDays) * 24 * time.Hour)
		}
	}
	return c.repository.PreviewRetentionJob(ctx, operation, actor.UserRef, cutoff)
}

func (c *Control) RunRetention(ctx context.Context, actor domain.User, operation, jobID string) (domain.RetentionJob, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.RetentionJob{}, ErrForbidden
	}
	if !validRetentionOperation(operation) || strings.TrimSpace(jobID) == "" {
		return domain.RetentionJob{}, domain.ErrInvalid
	}
	return c.repository.ConfirmRetentionJob(ctx, jobID, operation, actor.UserRef)
}

func (c *Control) RetentionJob(ctx context.Context, actor domain.User, jobID string) (domain.RetentionJob, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.RetentionJob{}, ErrForbidden
	}
	if strings.TrimSpace(jobID) == "" {
		return domain.RetentionJob{}, domain.ErrInvalid
	}
	job, err := c.repository.GetRetentionJob(ctx, jobID)
	if err != nil {
		return domain.RetentionJob{}, err
	}
	if job.ActorRef != actor.UserRef {
		return domain.RetentionJob{}, ErrForbidden
	}
	return job, nil
}

func validRetentionOperation(op string) bool {
	return op == "usage_details" || op == "closed_periods" || op == "reset_current_period"
}
