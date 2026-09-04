package domain

import (
	"encoding/json"
	"time"
)

type UserRole string

const (
	UserRoleAdmin     UserRole = "carpool_admin"
	UserRolePassenger UserRole = "passenger"
)

func (r UserRole) Valid() bool {
	return r == UserRoleAdmin || r == UserRolePassenger
}

type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

func (s UserStatus) Valid() bool {
	return s == UserStatusActive || s == UserStatusDisabled
}

type CarStatus string

const (
	CarStatusActive   CarStatus = "active"
	CarStatusDisabled CarStatus = "disabled"
	CarStatusRetired  CarStatus = "retired"
)

func (s CarStatus) Valid() bool {
	return s == CarStatusActive || s == CarStatusDisabled || s == CarStatusRetired
}

type RequestOutcome string

const (
	RequestOutcomeInProgress RequestOutcome = "in_progress"
	RequestOutcomeSucceeded  RequestOutcome = "succeeded"
	RequestOutcomeFailed     RequestOutcome = "failed"
	RequestOutcomeRejected   RequestOutcome = "rejected"
	RequestOutcomeCanceled   RequestOutcome = "canceled"
	RequestOutcomeIncomplete RequestOutcome = "incomplete"
)

func (o RequestOutcome) Valid() bool {
	switch o {
	case RequestOutcomeInProgress, RequestOutcomeSucceeded, RequestOutcomeFailed,
		RequestOutcomeRejected, RequestOutcomeCanceled, RequestOutcomeIncomplete:
		return true
	default:
		return false
	}
}

func (o RequestOutcome) Terminal() bool {
	return o.Valid() && o != RequestOutcomeInProgress
}

type User struct {
	ID                 string
	UserRef            string
	Username           string
	UsernameNormalized string
	DefaultDisplayName string
	Role               UserRole
	Status             UserStatus
	PasswordHash       string
	PasswordVersion    int64
	MustChangePassword bool
	DisabledAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Session struct {
	ID              string
	UserID          string
	TokenDigest     []byte
	CSRFToken       string
	PasswordVersion int64
	CreatedAt       time.Time
	LastSeenAt      time.Time
	ExpiresAt       time.Time
	RevokedAt       *time.Time
	RevokeReason    string
}

type APIKey struct {
	KeyID        string
	UserID       string
	Name         string
	SecretDigest []byte
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	LastUsedAt   *time.Time
	RevokedAt    *time.Time
	RevokeReason string
}

type Car struct {
	ID          string
	CarRef      string
	Name        string
	Description string
	SeatLimit   *int
	Status      CarStatus
	Version     int64
	DisabledAt  *time.Time
	RetiredAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Membership struct {
	ID              string
	MemberRef       string
	UserID          string
	CarID           string
	DisplayName     string
	DisplayNameKey  string
	StartedAt       time.Time
	EndedAt         *time.Time
	EndedReason     string
	CreatedByUserID string
}

type MembershipMove struct {
	Membership
	ExpectedCurrentID string
	EndCurrentReason  string
	Audit             *AuditEvent
}

type AuthAssignment struct {
	ID               string
	AccountRef       string
	CarID            string
	AuthID           string
	SafeLabel        string
	SafeLabelKey     string
	ProviderSnapshot string
	StartedAt        time.Time
	EndedAt          *time.Time
	EndedReason      string
	CreatedByUserID  string
}

type AuthAssignmentMove struct {
	AuthAssignment
	ExpectedCurrentID string
	EndCurrentReason  string
	Audit             *AuditEvent
}

type ProxyRequest struct {
	RequestID           string
	UserID              string
	APIKeyID            string
	CarID               string
	MembershipID        string
	MemberRefSnapshot   string
	DisplayNameSnapshot string
	ScopeHash           string
	ScopeSize           int
	SourceFormat        string
	RequestedModel      string
	Stream              bool
	StartedAt           time.Time
	CompletedAt         *time.Time
	Outcome             RequestOutcome
	StatusClass         string
	ReasonCode          string
	UpstreamAttempted   bool
}

type ProxyRequestAuthScope struct {
	RequestID          string
	AuthID             string
	AssignmentID       string
	AccountRefSnapshot string
	SafeLabelSnapshot  string
	ProviderSnapshot   string
}

type RequestCompletion struct {
	RequestID         string
	RequestedModel    string
	Stream            bool
	Outcome           RequestOutcome
	CompletedAt       time.Time
	StatusClass       string
	ReasonCode        string
	UpstreamAttempted bool
}

type UsageEvent struct {
	EventID            string
	RequestID          string
	EventSeq           *int64
	AttemptNo          *int64
	AuthID             string
	AssignmentID       string
	AccountRefSnapshot string
	SafeLabelSnapshot  string
	Provider           string
	Model              string
	UsageKnown         bool
	InputTokens        *int64
	OutputTokens       *int64
	CachedTokens       *int64
	ReasoningTokens    *int64
	TotalTokens        *int64
	Failed             bool
	StatusClass        string
	RequestedAt        time.Time
	RecordedAt         time.Time
}

type AuditEvent struct {
	ID           string
	OccurredAt   time.Time
	RequestID    string
	ActorType    string
	ActorRef     string
	Action       string
	TargetType   string
	TargetRef    string
	Result       string
	ReasonCode   string
	MetadataJSON json.RawMessage
}

type ProxyAuthorization struct {
	RequestID           string
	UserID              string
	APIKeyID            string
	SourceFormat        string
	RequestedModel      string
	Stream              bool
	StartedAt           time.Time
	RuntimeAuthIDs      []string
	PreflightReasonCode string
}

type AuthorizationSnapshot struct {
	Request    ProxyRequest
	User       User
	APIKey     APIKey
	Membership *Membership
	Car        *Car
	Scopes     []ProxyRequestAuthScope
}

type MemberUsageAggregate struct {
	MembershipID      string
	MemberRef         string
	DisplayName       string
	RequestCount      int64
	SucceededCount    int64
	FailedCount       int64
	RejectedCount     int64
	CanceledCount     int64
	IncompleteCount   int64
	KnownInputTokens  int64
	KnownOutputTokens int64
	KnownTotalTokens  int64
	UnknownUsageCount int64
}

type AccountUsageAggregate struct {
	AssignmentID      string
	AccountRef        string
	SafeLabel         string
	Provider          string
	RequestCount      int64
	KnownInputTokens  int64
	KnownOutputTokens int64
	KnownTotalTokens  int64
	UnknownUsageCount int64
}

type AdminUsageGroup string

const (
	AdminUsageGroupCar     AdminUsageGroup = "car"
	AdminUsageGroupUser    AdminUsageGroup = "user"
	AdminUsageGroupAccount AdminUsageGroup = "account"
)

func (g AdminUsageGroup) Valid() bool {
	return g == AdminUsageGroupCar || g == AdminUsageGroupUser || g == AdminUsageGroupAccount
}

// AdminUsageFilter contains only resolved database identifiers.
type AdminUsageFilter struct {
	From         time.Time
	To           time.Time
	CarID        string
	UserID       string
	AssignmentID string
	GroupBy      AdminUsageGroup
}

// AdminUsageAggregate is a safe grouped projection for administrator reports.
type AdminUsageAggregate struct {
	GroupBy              AdminUsageGroup
	GroupRef             string
	GroupLabel           string
	Provider             string
	RequestCount         int64
	InProgressCount      int64
	SucceededCount       int64
	FailedCount          int64
	RejectedCount        int64
	CanceledCount        int64
	IncompleteCount      int64
	KnownInputTokens     int64
	KnownOutputTokens    int64
	KnownCachedTokens    int64
	KnownReasoningTokens int64
	KnownTotalTokens     int64
	UnknownUsageCount    int64
}
