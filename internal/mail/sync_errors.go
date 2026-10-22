package mail

import "errors"

type syncStepError struct {
	step string
	err  error
}

func (e *syncStepError) Error() string         { return e.step + "失败" }
func (e *syncStepError) PublicMessage() string { return e.Error() }
func (e *syncStepError) Unwrap() error         { return e.err }

func syncStep(step string, err error) error {
	if err == nil {
		return nil
	}
	return &syncStepError{step: step, err: err}
}

// Only explicitly safe errors may be displayed or logged. SQLite and remote
// errors can otherwise contain message values, usernames or credentials.
func syncErrorMessage(err error) string {
	var safe interface{ PublicMessage() string }
	if errors.As(err, &safe) {
		return safe.PublicMessage()
	}
	return "同步处理失败"
}
