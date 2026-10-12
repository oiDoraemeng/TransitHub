package upstream

type RequestError struct {
	MessageKey string
	Platform   Platform
	// StatusCode 是上游返回的 HTTP 状态码（仅非 2xx 响应错误会填充，其它错误为 0）。
	// 现有调用方只读 MessageKey，新增此字段向后兼容；需要区分 403/401 等细分场景的调用方
	// （如 new-api channel key 获取的安全验证判定）可读取它。
	StatusCode int
	// Reason 是上游错误响应中的稳定机器码。它只用于协议兼容判断，
	// 不向用户展示，也不会包含请求凭证或响应正文。
	Reason string
}

func (e *RequestError) Error() string {
	return e.MessageKey
}

func newRequestError(messageKey string, platform Platform) *RequestError {
	return &RequestError{MessageKey: messageKey, Platform: platform}
}

func newRequestErrorWithStatus(messageKey string, platform Platform, statusCode int) *RequestError {
	return &RequestError{MessageKey: messageKey, Platform: platform, StatusCode: statusCode}
}

func newRequestErrorWithDetails(messageKey string, platform Platform, statusCode int, reason string) *RequestError {
	return &RequestError{MessageKey: messageKey, Platform: platform, StatusCode: statusCode, Reason: reason}
}

func errorKey(err error) string {
	if requestErr, ok := err.(*RequestError); ok {
		return requestErr.MessageKey
	}
	return ErrorUnknown
}
