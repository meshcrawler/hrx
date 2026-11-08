package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Git invokes this through credential.helper. Only get can disclose a credential;
// store/erase deliberately do nothing, so tokens never reach a credential store.
func gitCredential(args []string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("invalid credential helper operation")
	}
	if args[0] == "store" || args[0] == "erase" {
		return nil
	}
	if args[0] != "get" {
		return errors.New("invalid credential helper operation")
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(in, 16385))
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > 16384 {
			return errors.New("credential request too large")
		}
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("invalid credential request")
		}
		// Git repeats array attributes (notably wwwauth[] and capability[]).
		// Only the scalar routing fields affect where this helper sends a token.
		if key != "protocol" && key != "host" && key != "username" {
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return errors.New("duplicate credential field")
		}
		fields[key] = value
	}
	if scanner.Err() != nil {
		return errors.New("invalid credential request")
	}
	launch := currentGitlabLaunch()
	host, user, token := launch.Host, launch.Username, launch.Token
	if host == "" || user == "" || strings.ContainsAny(host+user+token, "\x00\r\n") ||
		fields["protocol"] != "https" || fields["host"] != host || (fields["username"] != "" && fields["username"] != user) {
		_, err := io.WriteString(out, "quit=true\n\n")
		return err
	}
	// The launch token expires; prefer a fresh one for the same identity and account.
	if fresh := refreshedBrokerToken(launch); fresh != "" {
		token = fresh
	}
	if token == "" {
		_, err := io.WriteString(out, "quit=true\n\n")
		return err
	}
	_, err := fmt.Fprintf(out, "username=%s\npassword=%s\n\n", user, token)
	return err
}

func gitCredentialEnvironment(env []string, executable, host, username string, aliases []string) ([]string, error) {
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	count := 0
	if value := values["GIT_CONFIG_COUNT"]; value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 0 || count > 1000 {
			return nil, errors.New("invalid inherited GIT_CONFIG_COUNT")
		}
		for i := 0; i < count; i++ {
			if _, ok := values[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]; !ok {
				return nil, errors.New("incomplete inherited Git configuration")
			}
			if _, ok := values[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]; !ok {
				return nil, errors.New("incomplete inherited Git configuration")
			}
		}
	}
	add := func(key, value string) {
		values[fmt.Sprintf("GIT_CONFIG_KEY_%d", count)] = key
		values[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)] = value
		count++
	}
	// Reset inherited helpers before adding ours: no Keychain, credential manager,
	// or personal login fallback, even when an HTTPS request fails authentication.
	add("credential.helper", "")
	add("credential.helper", "!"+"'"+strings.ReplaceAll(executable, "'", "'\\''")+"' _git-credential")
	add("credential.interactive", "false")
	add("credential.https://"+host+".username", username)
	add("core.askPass", "/usr/bin/false")
	add("http.https://"+host+"/.sslVerify", "true")
	add("http.https://"+host+"/.followRedirects", "false")
	add("http.https://"+host+"/.extraHeader", "")
	for _, alias := range append([]string{host}, aliases...) {
		add("url.https://"+host+"/.insteadOf", "git@"+alias+":")
		add("url.https://"+host+"/.insteadOf", "ssh://git@"+alias+"/")
		add("url.https://"+host+"/.insteadOf", "ssh://git@"+alias+":22/")
	}
	values["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	values["GIT_TERMINAL_PROMPT"] = "0"
	values["GIT_SSL_NO_VERIFY"] = "false"
	values["GIT_ASKPASS"], values["SSH_ASKPASS"] = "/usr/bin/false", "/usr/bin/false"
	values["GCM_INTERACTIVE"] = "never"
	values["GIT_SSH_COMMAND"] = "ssh -o BatchMode=yes"
	values[envCredentialHost], values[envCredentialUser] = host, username
	// GIT_CONFIG_PARAMETERS can override the indexed configuration. Trace output
	// can contain authentication details, so neither is inherited by the harness.
	for key := range values {
		if key == "GIT_CONFIG_PARAMETERS" || strings.HasPrefix(key, "GIT_TRACE") || key == "GIT_CURL_VERBOSE" {
			delete(values, key)
		}
	}
	return childEnvironment(nil, values, nil), nil
}
