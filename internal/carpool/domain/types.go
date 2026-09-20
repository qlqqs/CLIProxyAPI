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
	Token        string
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
	// MonthlyLimitNanoUSD is retained only for historical compatibility.
	MonthlyLimitNanoUSD  *int64
	FiveHourLimitNanoUSD *int64
	WeeklyLimitNanoUSD   *int64
	BillingTimezone      string
	BillingAnchorAt      time.Time
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
	BillingPeriodID     string
	PricingCatalogHash  string
	PricingCoverageFrom *time.Time
	BillingStatus       string
	BilledNanoUSD       *int64
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
	EventID             string
	RequestID           string
	BillingPeriodID     string
	EventSeq            *int64
	AttemptNo           *int64
	AuthID              string
	AssignmentID        string
	AccountRefSnapshot  string
	SafeLabelSnapshot   string
	Provider            string
	Model               string
	UsageKnown          bool
	InputTokens         *int64
	OutputTokens        *int64
	CachedTokens        *int64
	ReasoningTokens     *int64
	TotalTokens         *int64
	Failed              bool
	StatusClass         string
	RequestedAt         time.Time
	RecordedAt          time.Time
	CanonicalSchema     int
	CanonicalQuality    string
	UncachedInputTokens *int64
	CacheReadTokens     *int64
	CacheWriteTokens    *int64
	NonReasoningTokens  *int64
	RequestServiceTier  string
	ResponseServiceTier string
	PricingStatus       string
	PricingReason       string
	PriceInputPerToken  string
	PriceOutputPerToken string
	PriceCacheRead      string
	PriceCacheWrite     string
	CostNanoUSD         *int64
}

// BillingPeriod is a monthly anniversary period represented in UTC.
type BillingPeriod struct {
	ID                   string
	MembershipID         string
	MemberRefSnapshot    string
	CarID                string
	Timezone             string
	AnchorAt             time.Time
	From                 time.Time
	To                   time.Time
	LimitNanoUSD         *int64
	ConfirmedNanoUSD     int64
	ResetBaselineNanoUSD int64
	UnknownCostEvents    int64
	Revision             int64
}

// BillingSnapshot is the safe amount projection returned by browser APIs.
type BillingSnapshot struct {
	Currency          string
	Status            string
	LimitNanoUSD      *int64
	ConfirmedNanoUSD  int64
	RemainingNanoUSD  *int64
	OverageNanoUSD    int64
	UsagePercent      *int64
	UnknownCostEvents int64
	DataComplete      bool
	PeriodFrom        time.Time
	PeriodTo          time.Time
	Timezone          string
	CoverageFrom      *time.Time
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
	// CheckOnly validates admission without persisting a request or audit.
	CheckOnly            bool
	ExpectedMembershipID string
	ExpectedAuthID       string
	// NonBillable is set only by the server for model metadata reads.
	NonBillable         bool
	RequestID           string
	UserID              string
	APIKeyID            string
	SourceFormat        string
	RequestedModel      string
	Stream              bool
	StartedAt           time.Time
	RuntimeAuthIDs      []string
	PreflightReasonCode string
	PricingCatalogHash  string
	PricingCoverageFrom *time.Time
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

// UsageRequestDetail is a safe, one-row logical request projection. Events are
// returned separately so retries never inflate the logical request count.
type UsageRequestDetail struct {
	Request       ProxyRequest
	UserRef       string
	CarRef        string
	APIKeyRef     string
	EventCount    int64
	UnknownEvents int64
	Events        []UsageEvent
}

// UsageRequestFilter contains resolved identifiers and safe display filters.
type UsageRequestFilter struct {
	From           time.Time
	To             time.Time
	CarID          string
	UserID         string
	AssignmentID   string
	APIKeyID       string
	RequestedModel string
	Outcome        string
	BillingStatus  string
	RequestID      string
}

// RetentionSettings controls request detail retention. A nil override uses the
// configured default; zero means retain forever.
type RetentionSettings struct {
	OverrideDays  *int64
	EffectiveDays int64
	DefaultDays   int64
	Source        string
}

const RetentionBatchLimit = 1000
const RetentionPreviewTTL = 15 * time.Minute

type RetentionJob struct {
	ID            string
	Operation     string
	Status        string
	RequestedAt   time.Time
	CompletedAt   *time.Time
	ActorRef      string
	Confirmation  string
	ExpectedCount int64
	DeletedCount  int64
	InFlightCount int64
	FailureReason string
}
