package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

const (
	sessionCookieName = "cpa_carpool_session"
	csrfHeaderName    = "X-Carpool-CSRF"
	maximumJSONBody   = 1 << 20
	defaultPageLimit  = 50
	maximumPageLimit  = 200
)

const identityContextKey = "carpoolSessionIdentity"

// Config controls browser-facing security and cookie behavior.
type Config struct {
	CookieSecure    bool
	SessionTTL      time.Duration
	TrustedOrigins  []string
	TrustedProxyNet []*net.IPNet
	Now             func() time.Time
}

// API exposes the isolated carpool browser API.
type API struct {
	control         *carpoolservice.Control
	originValidator OriginValidator
	cookieSecure    bool
	sessionTTL      time.Duration
	trustedProxies  []*net.IPNet
	now             func() time.Time
}

type optionalJSON[T any] struct {
	Value T
	Set   bool
	Null  bool
}

func (value *optionalJSON[T]) UnmarshalJSON(data []byte) error {
	value.Set = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		value.Null = true
		return nil
	}
	return json.Unmarshal(data, &value.Value)
}

func New(control *carpoolservice.Control, cfg Config) (*API, error) {
	if control == nil {
		return nil, fmt.Errorf("carpool http API: control is required")
	}
	validator, errValidator := NewOriginValidator(cfg.TrustedOrigins)
	if errValidator != nil {
		return nil, errValidator
	}
	if cfg.SessionTTL <= 0 {
		return nil, fmt.Errorf("carpool http API: session TTL must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &API{control: control, originValidator: validator, cookieSecure: cfg.CookieSecure, sessionTTL: cfg.SessionTTL, trustedProxies: cfg.TrustedProxyNet, now: cfg.Now}, nil
}

// RegisterRoutes attaches only explicit API and static routes.
func (a *API) RegisterRoutes(engine *gin.Engine) {
	if a == nil || engine == nil {
		return
	}
	RegisterStaticRoutes(engine)

	root := engine.Group("/carpool/api/v1")
	root.Use(carpoolRequestID, noStore)
	root.POST("/session", a.requireOrigin(), a.login)
	root.GET("/session", a.requireSession(), a.session)
	root.DELETE("/session", a.requireSession(), a.requireMutation(), a.logout)
	root.POST("/me/password", a.requireSession(), a.requireMutation(), a.changePassword)

	authenticated := root.Group("")
	authenticated.Use(a.requireSession(), requireChangedPassword)
	authenticated.GET("/me/api-keys", requirePassenger, a.listMyAPIKeys)
	authenticated.POST("/me/api-keys", requirePassenger, a.requireMutation(), a.createMyAPIKey)
	authenticated.DELETE("/me/api-keys/:key_ref", requirePassenger, a.requireMutation(), a.revokeMyAPIKey)
	authenticated.GET("/me/car", requirePassenger, a.myCar)
	authenticated.GET("/me/members/usage", requirePassenger, a.myMemberUsage)
	authenticated.GET("/me/accounts", requirePassenger, a.myAccounts)

	admin := authenticated.Group("/admin")
	admin.Use(requireAdmin)
	admin.GET("/users", a.listUsers)
	admin.POST("/users", a.requireMutation(), a.createUser)
	admin.PATCH("/users/:user_ref", a.requireMutation(), a.updateUser)
	admin.POST("/users/:user_ref/reset-password", a.requireMutation(), a.resetPassword)
	admin.GET("/users/:user_ref/api-keys", a.listUserAPIKeys)
	admin.DELETE("/users/:user_ref/api-keys/:key_ref", a.requireMutation(), a.revokeUserAPIKey)
	admin.DELETE("/users/:user_ref/api-keys", a.requireMutation(), a.revokeUserAPIKeys)
	admin.GET("/cars", a.listCars)
	admin.POST("/cars", a.requireMutation(), a.createCar)
	admin.PATCH("/cars/:car_ref", a.requireMutation(), a.updateCar)
	admin.GET("/cars/:car_ref/members", a.listMembers)
	admin.POST("/cars/:car_ref/members", a.requireMutation(), a.moveMember)
	admin.DELETE("/cars/:car_ref/members/:member_ref", a.requireMutation(), a.removeMember)
	admin.GET("/cars/:car_ref/accounts", a.listAccounts)
	admin.POST("/cars/:car_ref/accounts", a.requireMutation(), a.moveAccount)
	admin.DELETE("/cars/:car_ref/accounts/:account_ref", a.requireMutation(), a.removeAccount)
	admin.GET("/usage", a.adminUsage)
	admin.GET("/audit-events", a.auditEvents)
}

// HandleNoRoute returns a JSON 404 for unknown carpool API paths.
func (a *API) HandleNoRoute(c *gin.Context) bool {
	if a == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	path := c.Request.URL.Path
	if path != "/carpool/api" && !strings.HasPrefix(path, "/carpool/api/") {
		return false
	}
	ensureCarpoolRequestID(c)
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	writeAPIError(c, http.StatusNotFound, "route_not_found", "接口不存在")
	c.Abort()
	return true
}

func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Next()
}

func carpoolRequestID(c *gin.Context) {
	ensureCarpoolRequestID(c)
	c.Next()
}

func ensureCarpoolRequestID(c *gin.Context) {
	requestID := logging.GetGinRequestID(c)
	if requestID == "" {
		requestID = logging.GenerateRequestID()
		logging.SetGinRequestID(c, requestID)
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), requestID))
	}
	c.Header("X-Request-ID", requestID)
}

func (a *API) requireOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.originValidator.Allows(c.Request) {
			writeAPIError(c, http.StatusForbidden, "origin_not_allowed", "请求来源不受信任")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (a *API) requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, errCookie := c.Cookie(sessionCookieName)
		if errCookie != nil || token == "" {
			writeAPIError(c, http.StatusUnauthorized, "session_required", "请先登录")
			c.Abort()
			return
		}
		identity, errIdentity := a.control.AuthenticateSession(c.Request.Context(), token)
		if errIdentity != nil {
			if !errors.Is(errIdentity, carpoolservice.ErrUnauthenticated) {
				log.WithError(errIdentity).Error("carpool session validation failed")
			}
			a.clearSessionCookie(c)
			writeAPIError(c, http.StatusUnauthorized, "session_invalid", "会话已失效")
			c.Abort()
			return
		}
		c.Set(identityContextKey, identity)
		c.Next()
	}
}

func (a *API) requireMutation() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !a.originValidator.Allows(c.Request) {
			writeAPIError(c, http.StatusForbidden, "origin_not_allowed", "请求来源不受信任")
			c.Abort()
			return
		}
		identity, ok := currentIdentity(c)
		if !ok || !CSRFTokenEqual(identity.Session.CSRFToken, c.GetHeader(csrfHeaderName)) {
			writeAPIError(c, http.StatusForbidden, "csrf_invalid", "CSRF 校验失败")
			c.Abort()
			return
		}
		c.Next()
	}
}

func requireChangedPassword(c *gin.Context) {
	identity, ok := currentIdentity(c)
	if !ok {
		writeAPIError(c, http.StatusUnauthorized, "session_required", "请先登录")
		c.Abort()
		return
	}
	if identity.User.MustChangePassword {
		writeAPIError(c, http.StatusForbidden, "password_change_required", "必须先修改临时密码")
		c.Abort()
		return
	}
	c.Next()
}

func requireAdmin(c *gin.Context) {
	identity, ok := currentIdentity(c)
	if !ok || identity.User.Role != domain.UserRoleAdmin {
		writeAPIError(c, http.StatusForbidden, "forbidden", "无权执行此操作")
		c.Abort()
		return
	}
	c.Next()
}

func requirePassenger(c *gin.Context) {
	identity, ok := currentIdentity(c)
	if !ok || identity.User.Role != domain.UserRolePassenger {
		writeAPIError(c, http.StatusForbidden, "forbidden", "无权执行此操作")
		c.Abort()
		return
	}
	c.Next()
}

func (a *API) login(c *gin.Context) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	clientAddress := TrustedClientAddress(c.Request, a.trustedProxies)
	result, errLogin := a.control.Login(c.Request.Context(), request.Username, request.Password, clientAddress)
	if errLogin != nil {
		var rateLimit *carpoolservice.LoginRateLimitError
		if errors.As(errLogin, &rateLimit) {
			retrySeconds := int64((rateLimit.RetryAfter + time.Second - 1) / time.Second)
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			c.Header("Retry-After", strconv.FormatInt(retrySeconds, 10))
			writeAPIError(c, http.StatusTooManyRequests, "rate_limited", "登录尝试过于频繁")
			return
		}
		if errors.Is(errLogin, carpoolservice.ErrUnauthenticated) {
			writeAPIError(c, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
			return
		}
		writeMappedError(c, errLogin)
		return
	}
	a.setSessionCookie(c, result.Token, result.Identity.Session.ExpiresAt)
	c.JSON(http.StatusCreated, sessionResponse(result.Identity, a.control.ReportLocationName()))
}

func (a *API) session(c *gin.Context) {
	identity, _ := currentIdentity(c)
	c.JSON(http.StatusOK, sessionResponse(identity, a.control.ReportLocationName()))
}

func (a *API) logout(c *gin.Context) {
	identity, _ := currentIdentity(c)
	if errLogout := a.control.Logout(c.Request.Context(), identity); errLogout != nil {
		writeMappedError(c, errLogout)
		return
	}
	a.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (a *API) changePassword(c *gin.Context) {
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	if errChange := a.control.ChangePassword(c.Request.Context(), identity, request.CurrentPassword, request.NewPassword); errChange != nil {
		if errors.Is(errChange, carpoolservice.ErrUnauthenticated) {
			writeAPIError(c, http.StatusUnauthorized, "invalid_credentials", "当前密码错误")
			return
		}
		writeMappedError(c, errChange)
		return
	}
	a.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

func (a *API) listMyAPIKeys(c *gin.Context) {
	cursor, limit, ok := paginationQuery(c)
	if !ok {
		return
	}
	identity, _ := currentIdentity(c)
	page, errKeys := a.control.ListAPIKeys(c.Request.Context(), identity.User, identity.User, cursor, limit)
	if errKeys != nil {
		writeMappedError(c, errKeys)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, key := range page.Items {
		items = append(items, a.apiKeyResponse(key))
	}
	c.JSON(http.StatusOK, pageResponse(items, page.Total, page.NextCursor))
}

func (a *API) createMyAPIKey(c *gin.Context) {
	var request struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	result, errCreate := a.control.CreateAPIKey(c.Request.Context(), identity.User, request.Name, request.ExpiresAt)
	if errCreate != nil {
		writeMappedError(c, errCreate)
		return
	}
	response := a.apiKeyResponse(result.APIKey)
	response["api_key"] = result.Token
	c.JSON(http.StatusCreated, response)
}

func (a *API) revokeMyAPIKey(c *gin.Context) {
	identity, _ := currentIdentity(c)
	if errRevoke := a.control.RevokeAPIKey(c.Request.Context(), identity.User, identity.User, c.Param("key_ref")); errRevoke != nil {
		writeMappedError(c, errRevoke)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *API) myCar(c *gin.Context) {
	identity, _ := currentIdentity(c)
	summary, _, errCar := a.control.PassengerCarSummary(c.Request.Context(), identity.User)
	if errors.Is(errCar, domain.ErrNotFound) {
		c.JSON(http.StatusOK, gin.H{"car": nil, "report_timezone": a.control.ReportLocationName()})
		return
	}
	if errCar != nil {
		writeMappedError(c, errCar)
		return
	}
	c.JSON(http.StatusOK, gin.H{"car": carResponse(summary), "report_timezone": a.control.ReportLocationName()})
}

func (a *API) myMemberUsage(c *gin.Context) {
	identity, _ := currentIdentity(c)
	period, views, errUsage := a.control.PassengerMembers(c.Request.Context(), identity.User, reportPeriod(c))
	if errUsage != nil {
		writeMappedError(c, errUsage)
		return
	}
	items := make([]gin.H, 0, len(views))
	unknown, incomplete := int64(0), int64(0)
	for _, view := range views {
		aggregate := view.Usage
		unknown += aggregate.UnknownUsageCount
		incomplete += aggregate.IncompleteCount
		items = append(items, memberUsageResponse(aggregate, view.Left))
	}
	c.JSON(http.StatusOK, reportResponse(period, items, unknown, incomplete))
}

func (a *API) myAccounts(c *gin.Context) {
	identity, _ := currentIdentity(c)
	period, views, errAccounts := a.control.PassengerAccounts(c.Request.Context(), identity.User, reportPeriod(c))
	if errAccounts != nil {
		writeMappedError(c, errAccounts)
		return
	}
	items := make([]gin.H, 0, len(views))
	for _, view := range views {
		items = append(items, gin.H{
			"account_ref": view.Assignment.AccountRef,
			"label":       view.Assignment.SafeLabel,
			"provider":    view.Assignment.ProviderSnapshot,
			"status":      view.Health.Status,
			"observed_at": optionalTime(view.Health.ObservedAt),
			"stale":       view.Health.Stale,
			"usage": gin.H{
				"logical_requests":     view.Usage.RequestCount,
				"known_input_tokens":   view.Usage.KnownInputTokens,
				"known_output_tokens":  view.Usage.KnownOutputTokens,
				"known_total_tokens":   view.Usage.KnownTotalTokens,
				"unknown_usage_events": view.Usage.UnknownUsageCount,
			},
		})
	}
	c.JSON(http.StatusOK, gin.H{"period": period.Name, "data_from": period.From, "data_to": period.To, "items": items})
}

func (a *API) listUsers(c *gin.Context) {
	cursor, limit, ok := paginationQuery(c)
	if !ok {
		return
	}
	identity, _ := currentIdentity(c)
	page, errUsers := a.control.ListUsers(c.Request.Context(), identity.User, cursor, limit)
	if errUsers != nil {
		writeMappedError(c, errUsers)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, user := range page.Items {
		items = append(items, userResponse(user))
	}
	c.JSON(http.StatusOK, pageResponse(items, page.Total, page.NextCursor))
}

func (a *API) createUser(c *gin.Context) {
	var request struct {
		Username    string          `json:"username"`
		DisplayName string          `json:"display_name"`
		Role        domain.UserRole `json:"role"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	result, errCreate := a.control.CreateUser(c.Request.Context(), identity.User, request.Username, request.DisplayName, request.Role)
	if errCreate != nil {
		writeMappedError(c, errCreate)
		return
	}
	response := userResponse(result.User)
	response["temporary_password"] = result.TemporaryPassword
	c.JSON(http.StatusCreated, response)
}

func (a *API) updateUser(c *gin.Context) {
	var request struct {
		DefaultDisplayName *string            `json:"default_display_name"`
		Status             *domain.UserStatus `json:"status"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	user, errUpdate := a.control.UpdateUser(c.Request.Context(), identity.User, c.Param("user_ref"), request.DefaultDisplayName, request.Status)
	if errUpdate != nil {
		writeMappedError(c, errUpdate)
		return
	}
	c.JSON(http.StatusOK, userResponse(user))
}

func (a *API) listUserAPIKeys(c *gin.Context) {
	cursor, limit, ok := paginationQuery(c)
	if !ok {
		return
	}
	identity, _ := currentIdentity(c)
	owner, errOwner := a.control.UserByRef(c.Request.Context(), identity.User, c.Param("user_ref"))
	if errOwner != nil {
		writeMappedError(c, errOwner)
		return
	}
	page, errKeys := a.control.ListAPIKeys(c.Request.Context(), identity.User, owner, cursor, limit)
	if errKeys != nil {
		writeMappedError(c, errKeys)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, key := range page.Items {
		items = append(items, a.apiKeyResponse(key))
	}
	c.JSON(http.StatusOK, pageResponse(items, page.Total, page.NextCursor))
}

func (a *API) revokeUserAPIKey(c *gin.Context) {
	identity, _ := currentIdentity(c)
	owner, errOwner := a.control.UserByRef(c.Request.Context(), identity.User, c.Param("user_ref"))
	if errOwner != nil {
		writeMappedError(c, errOwner)
		return
	}
	if errRevoke := a.control.RevokeAPIKey(c.Request.Context(), identity.User, owner, c.Param("key_ref")); errRevoke != nil {
		writeMappedError(c, errRevoke)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *API) resetPassword(c *gin.Context) {
	identity, _ := currentIdentity(c)
	password, errReset := a.control.ResetPassword(c.Request.Context(), identity.User, c.Param("user_ref"))
	if errReset != nil {
		writeMappedError(c, errReset)
		return
	}
	c.JSON(http.StatusOK, gin.H{"temporary_password": password})
}

func (a *API) revokeUserAPIKeys(c *gin.Context) {
	identity, _ := currentIdentity(c)
	owner, errOwner := a.control.UserByRef(c.Request.Context(), identity.User, c.Param("user_ref"))
	if errOwner != nil {
		writeMappedError(c, errOwner)
		return
	}
	if errRevoke := a.control.RevokeAllAPIKeys(c.Request.Context(), identity.User, owner); errRevoke != nil {
		writeMappedError(c, errRevoke)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *API) listCars(c *gin.Context) {
	cursor, limit, ok := paginationQuery(c)
	if !ok {
		return
	}
	identity, _ := currentIdentity(c)
	page, errCars := a.control.ListCars(c.Request.Context(), identity.User, cursor, limit)
	if errCars != nil {
		writeMappedError(c, errCars)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, car := range page.Items {
		items = append(items, carResponse(car))
	}
	c.JSON(http.StatusOK, pageResponse(items, page.Total, page.NextCursor))
}

func (a *API) createCar(c *gin.Context) {
	var request struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		SeatLimit   *int   `json:"seat_limit"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	car, errCreate := a.control.CreateCar(c.Request.Context(), identity.User, request.Name, request.Description, request.SeatLimit)
	if errCreate != nil {
		writeMappedError(c, errCreate)
		return
	}
	c.JSON(http.StatusCreated, carResponse(carpoolservice.CarSummary{Car: car}))
}

func (a *API) updateCar(c *gin.Context) {
	var request struct {
		Name        optionalJSON[string]           `json:"name"`
		Description optionalJSON[string]           `json:"description"`
		SeatLimit   optionalJSON[int]              `json:"seat_limit"`
		Status      optionalJSON[domain.CarStatus] `json:"status"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.Name.Null || request.Description.Null || request.Status.Null {
		writeMappedError(c, domain.ErrInvalid)
		return
	}
	update := carpoolservice.CarUpdate{SeatLimitSet: request.SeatLimit.Set}
	if request.Name.Set {
		update.Name = &request.Name.Value
	}
	if request.Description.Set {
		update.Description = &request.Description.Value
	}
	if request.SeatLimit.Set && !request.SeatLimit.Null {
		update.SeatLimit = &request.SeatLimit.Value
	}
	if request.Status.Set {
		update.Status = &request.Status.Value
	}
	identity, _ := currentIdentity(c)
	car, errUpdate := a.control.UpdateCar(c.Request.Context(), identity.User, c.Param("car_ref"), update)
	if errUpdate != nil {
		writeMappedError(c, errUpdate)
		return
	}
	c.JSON(http.StatusOK, carResponse(carpoolservice.CarSummary{Car: car}))
}

func (a *API) listMembers(c *gin.Context) {
	identity, _ := currentIdentity(c)
	members, errMembers := a.control.ListMembers(c.Request.Context(), identity.User, c.Param("car_ref"))
	if errMembers != nil {
		writeMappedError(c, errMembers)
		return
	}
	items := make([]gin.H, 0, len(members))
	for _, member := range members {
		items = append(items, gin.H{"member_ref": member.MemberRef, "display_name": member.DisplayName, "started_at": member.StartedAt})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

func (a *API) moveMember(c *gin.Context) {
	var request struct {
		UserRef     string `json:"user_ref"`
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	membership, errMove := a.control.MoveMember(c.Request.Context(), identity.User, c.Param("car_ref"), request.UserRef, request.DisplayName)
	if errMove != nil {
		writeMappedError(c, errMove)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"member_ref": membership.MemberRef, "display_name": membership.DisplayName, "started_at": membership.StartedAt})
}

func (a *API) removeMember(c *gin.Context) {
	identity, _ := currentIdentity(c)
	if errRemove := a.control.RemoveMember(c.Request.Context(), identity.User, c.Param("car_ref"), c.Param("member_ref")); errRemove != nil {
		writeMappedError(c, errRemove)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *API) listAccounts(c *gin.Context) {
	identity, _ := currentIdentity(c)
	accounts, errAccounts := a.control.ListAccounts(c.Request.Context(), identity.User, c.Param("car_ref"))
	if errAccounts != nil {
		writeMappedError(c, errAccounts)
		return
	}
	items := make([]gin.H, 0, len(accounts))
	for _, account := range accounts {
		items = append(items, adminAccountResponse(account))
	}
	candidates, errCandidates := a.control.ListAccountCandidates(c.Request.Context(), identity.User, c.Param("car_ref"))
	if errCandidates != nil {
		writeMappedError(c, errCandidates)
		return
	}
	candidateItems := make([]gin.H, 0, len(candidates))
	for _, candidate := range candidates {
		candidateItems = append(candidateItems, accountCandidateResponse(candidate))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items), "candidates": candidateItems})
}

func (a *API) moveAccount(c *gin.Context) {
	var request struct {
		CandidateRef string `json:"candidate_ref"`
		SafeLabel    string `json:"safe_label"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	identity, _ := currentIdentity(c)
	account, errMove := a.control.MoveAccountCandidate(c.Request.Context(), identity.User, c.Param("car_ref"), request.CandidateRef, request.SafeLabel)
	if errMove != nil {
		writeMappedError(c, errMove)
		return
	}
	c.JSON(http.StatusCreated, adminAccountResponse(account))
}

func (a *API) removeAccount(c *gin.Context) {
	identity, _ := currentIdentity(c)
	if errRemove := a.control.RemoveAccount(c.Request.Context(), identity.User, c.Param("car_ref"), c.Param("account_ref")); errRemove != nil {
		writeMappedError(c, errRemove)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *API) adminUsage(c *gin.Context) {
	query, ok := adminUsageQuery(c)
	if !ok {
		return
	}
	identity, _ := currentIdentity(c)
	report, errUsage := a.control.AdminUsage(c.Request.Context(), identity.User, query)
	if errUsage != nil {
		writeMappedError(c, errUsage)
		return
	}
	items := make([]gin.H, 0, len(report.Items))
	unknown, incomplete := int64(0), int64(0)
	for _, row := range report.Items {
		unknown += row.UnknownUsageCount
		incomplete += row.IncompleteCount
		items = append(items, adminUsageResponse(row))
	}
	response := reportResponse(report.Period, items, unknown, incomplete)
	response["retention_cutoff"] = report.RetentionCutoff
	response["group_by"] = report.GroupBy
	response["filters"] = gin.H{
		"car_ref":     optionalString(query.CarRef),
		"user_ref":    optionalString(query.UserRef),
		"account_ref": optionalString(query.AccountRef),
	}
	c.JSON(http.StatusOK, response)
}

func (a *API) auditEvents(c *gin.Context) {
	cursor, limit, ok := paginationQuery(c)
	if !ok {
		return
	}
	var before time.Time
	beforeID := strings.TrimSpace(c.Query("before_id"))
	if rawBefore := strings.TrimSpace(c.Query("before")); rawBefore != "" {
		var errBefore error
		before, errBefore = parseUTCTime(rawBefore)
		if errBefore != nil {
			writeAPIError(c, http.StatusUnprocessableEntity, "invalid_cursor", "分页游标无效")
			return
		}
	}
	identity, _ := currentIdentity(c)
	page, errEvents := a.control.ListAuditEvents(c.Request.Context(), identity.User, carpoolservice.AuditListQuery{
		Cursor: cursor, Before: before, BeforeID: beforeID, Limit: limit,
	})
	if errEvents != nil {
		writeMappedError(c, errEvents)
		return
	}
	items := make([]gin.H, 0, len(page.Items))
	for _, event := range page.Items {
		items = append(items, gin.H{"occurred_at": event.OccurredAt, "actor_type": event.ActorType, "actor_ref": event.ActorRef, "action": event.Action, "target_type": event.TargetType, "target_ref": event.TargetRef, "result": event.Result, "reason_code": event.ReasonCode})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": optionalString(page.NextCursor)})
}

func (a *API) setSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/carpool/", Expires: expiresAt, MaxAge: int(a.sessionTTL.Seconds()), HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteStrictMode})
}

func (a *API) clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/carpool/", Expires: time.Unix(1, 0), MaxAge: -1, HttpOnly: true, Secure: a.cookieSecure, SameSite: http.SameSiteStrictMode})
}

func currentIdentity(c *gin.Context) (carpoolservice.SessionIdentity, bool) {
	value, ok := c.Get(identityContextKey)
	if !ok {
		return carpoolservice.SessionIdentity{}, false
	}
	identity, ok := value.(carpoolservice.SessionIdentity)
	return identity, ok
}

func decodeJSON(c *gin.Context, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, maximumJSONBody+1))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(target); errDecode != nil {
		writeAPIError(c, http.StatusBadRequest, "invalid_json", "请求 JSON 无效")
		return false
	}
	if errExtra := decoder.Decode(&struct{}{}); !errors.Is(errExtra, io.EOF) {
		writeAPIError(c, http.StatusBadRequest, "invalid_json", "请求只能包含一个 JSON 值")
		return false
	}
	return true
}

func writeMappedError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, carpoolservice.ErrUnauthenticated):
		writeAPIError(c, http.StatusUnauthorized, "unauthenticated", "身份验证失败")
	case errors.Is(err, carpoolservice.ErrForbidden), errors.Is(err, domain.ErrAuthorizationRejected):
		writeAPIError(c, http.StatusForbidden, "forbidden", "无权执行此操作")
	case errors.Is(err, domain.ErrNotFound):
		writeAPIError(c, http.StatusNotFound, "not_found", "资源不存在")
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrAlreadyBootstrapped):
		writeAPIError(c, http.StatusConflict, "conflict", "当前状态与操作冲突")
	case errors.Is(err, domain.ErrInvalid):
		writeAPIError(c, http.StatusUnprocessableEntity, "invalid_request", "请求参数无效")
	case errors.Is(err, domain.ErrBusy):
		c.Header("Retry-After", "1")
		writeAPIError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "服务暂时繁忙")
	default:
		log.WithError(err).Error("carpool API request failed")
		writeAPIError(c, http.StatusInternalServerError, "internal_error", "服务内部错误")
	}
}

func writeAPIError(c *gin.Context, status int, code, message string) {
	errorBody := gin.H{"code": code, "message": message}
	if requestID := logging.GetGinRequestID(c); requestID != "" {
		errorBody["request_id"] = requestID
	}
	c.JSON(status, gin.H{"error": errorBody})
}

func sessionResponse(identity carpoolservice.SessionIdentity, timezone string) gin.H {
	return gin.H{"user_ref": identity.User.UserRef, "display_name": identity.User.DefaultDisplayName, "role": identity.User.Role, "must_change_password": identity.User.MustChangePassword, "csrf_token": identity.Session.CSRFToken, "expires_at": identity.Session.ExpiresAt, "module_status": "healthy", "report_timezone": timezone}
}

func userResponse(user domain.User) gin.H {
	return gin.H{"user_ref": user.UserRef, "username": user.Username, "display_name": user.DefaultDisplayName, "role": user.Role, "status": user.Status, "must_change_password": user.MustChangePassword, "created_at": user.CreatedAt, "updated_at": user.UpdatedAt}
}

func (a *API) apiKeyResponse(key domain.APIKey) gin.H {
	status := "active"
	if key.RevokedAt != nil {
		status = "revoked"
	} else if key.ExpiresAt != nil && !a.now().Before(*key.ExpiresAt) {
		status = "expired"
	}
	return gin.H{"key_ref": key.KeyID, "name": key.Name, "status": status, "created_at": key.CreatedAt, "expires_at": key.ExpiresAt, "last_used_at": key.LastUsedAt, "revoked_at": key.RevokedAt}
}

func carResponse(summary carpoolservice.CarSummary) gin.H {
	return gin.H{"car_ref": summary.Car.CarRef, "name": summary.Car.Name, "description": summary.Car.Description, "seat_limit": summary.Car.SeatLimit, "status": summary.Car.Status, "version": summary.Car.Version, "member_count": summary.MemberCount, "account_count": summary.AccountCount, "available_account_count": summary.AccountCount, "created_at": summary.Car.CreatedAt, "updated_at": summary.Car.UpdatedAt}
}

func adminAccountResponse(account domain.AuthAssignment) gin.H {
	return gin.H{"account_ref": account.AccountRef, "safe_label": account.SafeLabel, "provider": account.ProviderSnapshot, "started_at": account.StartedAt}
}

func accountCandidateResponse(candidate carpoolservice.AccountCandidate) gin.H {
	return gin.H{
		"candidate_ref":           candidate.CandidateRef,
		"provider":                candidate.Provider,
		"status":                  candidate.Health.Status,
		"observed_at":             optionalTime(candidate.Health.ObservedAt),
		"stale":                   candidate.Health.Stale,
		"assigned":                candidate.Assigned,
		"assigned_to_current_car": candidate.AssignedToCurrentCar,
	}
}

func memberUsageResponse(row domain.MemberUsageAggregate, left bool) gin.H {
	return gin.H{"member_ref": row.MemberRef, "display_name": row.DisplayName, "left": left, "logical_requests": row.RequestCount, "succeeded": row.SucceededCount, "failed": row.FailedCount, "rejected": row.RejectedCount, "canceled": row.CanceledCount, "incomplete": row.IncompleteCount, "known_input_tokens": row.KnownInputTokens, "known_output_tokens": row.KnownOutputTokens, "known_total_tokens": row.KnownTotalTokens, "unknown_usage_events": row.UnknownUsageCount}
}

func adminUsageResponse(row domain.AdminUsageAggregate) gin.H {
	response := gin.H{
		"logical_requests":       row.RequestCount,
		"in_progress":            row.InProgressCount,
		"succeeded":              row.SucceededCount,
		"failed":                 row.FailedCount,
		"rejected":               row.RejectedCount,
		"canceled":               row.CanceledCount,
		"incomplete":             row.IncompleteCount,
		"known_input_tokens":     row.KnownInputTokens,
		"known_output_tokens":    row.KnownOutputTokens,
		"known_cached_tokens":    row.KnownCachedTokens,
		"known_reasoning_tokens": row.KnownReasoningTokens,
		"known_total_tokens":     row.KnownTotalTokens,
		"unknown_usage_events":   row.UnknownUsageCount,
		"unattributed":           row.GroupRef == "",
	}
	switch row.GroupBy {
	case domain.AdminUsageGroupCar:
		response["car_ref"] = optionalString(row.GroupRef)
		response["name"] = optionalString(row.GroupLabel)
	case domain.AdminUsageGroupUser:
		response["user_ref"] = optionalString(row.GroupRef)
		response["display_name"] = optionalString(row.GroupLabel)
	case domain.AdminUsageGroupAccount:
		response["account_ref"] = optionalString(row.GroupRef)
		response["safe_label"] = optionalString(row.GroupLabel)
		response["provider"] = optionalString(row.Provider)
	}
	return response
}

func reportResponse(period carpoolservice.ReportPeriod, items []gin.H, unknown, incomplete int64) gin.H {
	return gin.H{"period": period.Name, "data_from": period.From, "data_to": period.To, "items": items, "coverage": gin.H{"unknown_usage_events": unknown, "incomplete_requests": incomplete}}
}

func reportPeriod(c *gin.Context) string {
	value := strings.TrimSpace(c.Query("period"))
	if value == "" {
		return "today"
	}
	return value
}

func paginationQuery(c *gin.Context) (string, int, bool) {
	cursor := strings.TrimSpace(c.Query("cursor"))
	rawLimit := strings.TrimSpace(c.Query("limit"))
	if rawLimit == "" {
		return cursor, defaultPageLimit, true
	}
	limit, errParse := strconv.Atoi(rawLimit)
	if errParse != nil || limit < 1 || limit > maximumPageLimit {
		writeAPIError(c, http.StatusUnprocessableEntity, "invalid_pagination", "分页参数无效")
		return "", 0, false
	}
	return cursor, limit, true
}

func pageResponse(items []gin.H, total int64, nextCursor string) gin.H {
	return gin.H{"items": items, "total": total, "next_cursor": optionalString(nextCursor)}
}

func adminUsageQuery(c *gin.Context) (carpoolservice.AdminUsageQuery, bool) {
	query := carpoolservice.AdminUsageQuery{
		Period:     strings.TrimSpace(c.Query("period")),
		CarRef:     strings.TrimSpace(c.Query("car_ref")),
		UserRef:    strings.TrimSpace(c.Query("user_ref")),
		AccountRef: strings.TrimSpace(c.Query("account_ref")),
		GroupBy:    domain.AdminUsageGroup(strings.TrimSpace(c.Query("group_by"))),
	}
	rawFrom, hasFrom := c.GetQuery("from")
	rawTo, hasTo := c.GetQuery("to")
	if hasFrom != hasTo || (hasFrom && query.Period != "") {
		writeAPIError(c, http.StatusUnprocessableEntity, "invalid_report_range", "报表时间范围无效")
		return carpoolservice.AdminUsageQuery{}, false
	}
	if !hasFrom {
		return query, true
	}
	from, errFrom := parseUTCTime(strings.TrimSpace(rawFrom))
	to, errTo := parseUTCTime(strings.TrimSpace(rawTo))
	if errFrom != nil || errTo != nil {
		writeAPIError(c, http.StatusUnprocessableEntity, "invalid_report_range", "报表时间范围必须使用 UTC RFC3339 时间")
		return carpoolservice.AdminUsageQuery{}, false
	}
	query.From = &from
	query.To = &to
	return query, true
}

func parseUTCTime(value string) (time.Time, error) {
	parsed, errParse := time.Parse(time.RFC3339Nano, value)
	if errParse != nil || parsed.IsZero() {
		return time.Time{}, domain.ErrInvalid
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return time.Time{}, domain.ErrInvalid
	}
	return parsed.UTC(), nil
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
