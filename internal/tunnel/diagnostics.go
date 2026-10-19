package tunnel

import "errors"

func (s StartupStage) String() string {
	switch s {
	case CheckingAccount:
		return "checking_account"
	case CreatingTunnel:
		return "creating_tunnel"
	case CheckingTunnel:
		return "checking_tunnel"
	case StartingHost:
		return "starting_host"
	case CheckingAddress:
		return "checking_address"
	case CheckingAccess:
		return "checking_access"
	default:
		return "unknown"
	}
}

func failureCode(err error) string {
	for _, item := range []struct {
		err  error
		code string
	}{
		{ErrCleanup, "cleanup_failed"}, {ErrNotLoggedIn, "not_logged_in"},
		{ErrUnverified, "configuration_not_verified"}, {ErrNotReady, "readiness_timeout"},
		{ErrHostExited, "host_exited"}, {ErrExecutable, "invalid_executable"},
		{ErrCommand, "command_failed"}, {ErrOptions, "invalid_options"},
	} {
		if errors.Is(err, item.err) {
			return item.code
		}
	}
	return "startup_interrupted"
}
