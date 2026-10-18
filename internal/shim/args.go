package shim

// IsStdioServer only intercepts known, plain app-server invocations. Utility
// subcommands and non-stdio transports must retain their original CLI behavior.
func IsStdioServer(args []string) bool {
	position := 0
	for position < len(args) {
		switch args[position] {
		case "-c", "--config", "--enable", "--disable":
			position += 2
		case "--strict-config":
			position++
		case "app-server":
			position++
			goto server
		default:
			return false
		}
	}
	return false
server:
	for position < len(args) {
		switch args[position] {
		case "--stdio", "--analytics-default-enabled", "--strict-config":
			position++
		case "--listen":
			if position+1 >= len(args) || args[position+1] != "stdio://" {
				return false
			}
			position += 2
		case "-c", "--config", "--enable", "--disable", "--code-mode-host":
			if position+1 >= len(args) {
				return false
			}
			position += 2
		default:
			return false
		}
	}
	return true
}
