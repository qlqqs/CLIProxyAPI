package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

// ProxyAdmissionError retains a safe rejection reason without exposing storage errors.
type ProxyAdmissionError struct {
	Reason string
	Cause  error
}

func (e *ProxyAdmissionError) Error() string { return "carpool: " + e.Reason }
func (e *ProxyAdmissionError) Unwrap() error { return e.Cause }

func (c *Control) memberLimitsView(ctx context.Context, member domain.Membership) (domain.MemberQuotaLimits, []domain.MemberQuotaWindow, error) {
	limits, errLimits := c.repository.GetMemberQuotaLimits(ctx, member.ID)
	if errLimits != nil {
		return limits, nil, errLimits
	}
	windows, errWindows := c.repository.MemberQuotaWindows(ctx, member.ID, c.currentTime())
	return limits, windows, errWindows
}

// PassengerQuota returns the current member's local rolling quota state.
func (c *Control) PassengerQuota(ctx context.Context, user domain.User) (domain.MemberQuotaLimits, []domain.MemberQuotaWindow, error) {
	_, member, errMember := c.PassengerCar(ctx, user)
	if errMember != nil {
		return domain.MemberQuotaLimits{}, nil, errMember
	}
	return c.memberLimitsView(ctx, member)
}

// SetMemberLimits applies explicitly supplied member limits atomically.
func (c *Control) SetMemberLimits(ctx context.Context, actor domain.User, carRef, memberRef string, update domain.MemberLimitsUpdate) (domain.Membership, domain.MemberQuotaLimits, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.Membership{}, domain.MemberQuotaLimits{}, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return domain.Membership{}, domain.MemberQuotaLimits{}, errCar
	}
	members, errMembers := c.repository.ListCurrentMembershipsByCar(ctx, car.ID)
	if errMembers != nil {
		return domain.Membership{}, domain.MemberQuotaLimits{}, errMembers
	}
	for _, member := range members {
		if member.MemberRef != strings.TrimSpace(memberRef) {
			continue
		}
		audit := c.audit(sessionAuditActorType, actor.UserRef, "update_member_limits", "membership", member.MemberRef, "succeeded", "")
		c.policyMu.Lock()
		errUpdate := c.repository.SetMemberLimits(ctx, member.ID, update, &audit)
		if errUpdate == nil && update.UserConcurrencySet {
			c.concurrency.SetUserLimit(member.UserID, update.UserConcurrency)
		}
		c.policyMu.Unlock()
		if errUpdate != nil {
			return domain.Membership{}, domain.MemberQuotaLimits{}, errUpdate
		}
		limits, errLimits := c.repository.GetMemberQuotaLimits(ctx, member.ID)
		return member, limits, errLimits
	}
	return domain.Membership{}, domain.MemberQuotaLimits{}, domain.ErrNotFound
}

// AccountConcurrency returns a policy using a public assignment reference.
func (c *Control) AccountConcurrency(ctx context.Context, actor domain.User, carRef, accountRef string) (*int, error) {
	assignment, errAssignment := c.adminAccount(ctx, actor, carRef, accountRef)
	if errAssignment != nil {
		return nil, errAssignment
	}
	return c.repository.GetAccountConcurrency(ctx, assignment.AuthID)
}

func (c *Control) adminAccount(ctx context.Context, actor domain.User, carRef, accountRef string) (domain.AuthAssignment, error) {
	if actor.Role != domain.UserRoleAdmin {
		return domain.AuthAssignment{}, ErrForbidden
	}
	car, errCar := c.carByRef(ctx, carRef)
	if errCar != nil {
		return domain.AuthAssignment{}, errCar
	}
	assignment, errAssignment := c.repository.GetAuthAssignmentByRef(ctx, strings.TrimSpace(accountRef))
	if errAssignment != nil {
		return domain.AuthAssignment{}, errAssignment
	}
	if assignment.CarID != car.ID || assignment.EndedAt != nil {
		return domain.AuthAssignment{}, domain.ErrNotFound
	}
	return assignment, nil
}

// SetAccountConcurrency keeps account policy stable across assignment changes.
func (c *Control) SetAccountConcurrency(ctx context.Context, actor domain.User, carRef, accountRef string, limit *int) error {
	if limit != nil && *limit <= 0 {
		return domain.ErrInvalid
	}
	assignment, errAssignment := c.adminAccount(ctx, actor, carRef, accountRef)
	if errAssignment != nil {
		return errAssignment
	}
	audit := c.audit(sessionAuditActorType, actor.UserRef, "update_account_concurrency", "account", assignment.AccountRef, "succeeded", "")
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	if errSet := c.repository.SetAccountConcurrency(ctx, assignment.AuthID, limit, &audit); errSet != nil {
		return errSet
	}
	c.concurrency.SetAccountLimit(assignment.AuthID, limit)
	return nil
}

func (c *Control) loadConcurrencyPolicy(ctx context.Context, userID, authID string) error {
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	userLimit, errUser := c.repository.GetUserConcurrency(ctx, userID)
	if errUser != nil {
		return errUser
	}
	var accountLimit *int
	if authID != "" {
		var errAccount error
		accountLimit, errAccount = c.repository.GetAccountConcurrency(ctx, authID)
		if errAccount != nil {
			return errAccount
		}
	}
	c.concurrency.SetUserLimit(userID, userLimit)
	if authID != "" {
		c.concurrency.SetAccountLimit(authID, accountLimit)
	}
	return nil
}

// CloseAdmission wakes queued callers; already admitted requests retain their permits.
func (c *Control) CloseAdmission() {
	if c != nil && c.concurrency != nil {
		c.concurrency.Close()
	}
}

func admissionResult(snapshot domain.AuthorizationSnapshot, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrAuthorizationRejected) && snapshot.Request.ReasonCode != "" {
		return &ProxyAdmissionError{Reason: snapshot.Request.ReasonCode, Cause: err}
	}
	return fmt.Errorf("carpool: proxy admission: %w", err)
}

// checkMultiAccountConcurrency preserves unrestricted multi-account requests,
// but never silently bypasses a cap without a single-account permit.
func (c *Control) checkMultiAccountConcurrency(ctx context.Context, scopes []domain.ProxyRequestAuthScope) error {
	if len(scopes) <= 1 {
		return nil
	}
	for _, scope := range scopes {
		limit, errLimit := c.repository.GetAccountConcurrency(ctx, scope.AuthID)
		if errLimit != nil {
			return errLimit
		}
		if limit != nil {
			return &ProxyAdmissionError{Reason: "account_concurrency_requires_single_account", Cause: domain.ErrAuthorizationRejected}
		}
	}
	return nil
}
