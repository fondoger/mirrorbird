package tunnel

import (
	"errors"
	"io"
	"strings"
	"testing"
)

const readyText = "Hosting port: 43127\r\nConnect via browser: " + testOrigin + "/\r\n\r\nReady to accept connections for tunnel: " + testID + "\r\n"

// Synthetic fixtures only; never copy a real authentication code into tests.
const testLoginURL = "https://login.microsoft.com/device"
const testLoginCode = "EXAMPLE12"
const testLoginText = "To sign in, use a web browser to open the page " + testLoginURL + " and enter the code " + testLoginCode + " to authenticate.\r\n"

func TestReadinessRequiresCompleteMatchingSignals(t *testing.T) {
	id, _ := parseID(testID, true)
	for _, chunkSize := range []int{1, 2, 13, len(readyText)} {
		output := newHostOutput(id, testPort)
		for offset := 0; offset < len(readyText); offset += chunkSize {
			chunk := readyText[offset:min(offset+chunkSize, len(readyText))]
			if n, err := io.WriteString(output, chunk); n != len(chunk) || err != nil {
				t.Fatal("partial stdout write")
			}
		}
		select {
		case result := <-output.ready:
			if result.err != nil || result.origin != testOrigin {
				t.Fatalf("chunk=%d result=%+v", chunkSize, result)
			}
		default:
			t.Fatalf("no readiness for chunk=%d", chunkSize)
		}
		_, _ = io.WriteString(output, "secret post-start CLI trace\n"+readyText)
		if output.line != nil {
			t.Fatal("kept raw output after readiness")
		}
		select {
		case <-output.ready:
			t.Fatal("readiness sent twice")
		default:
		}
	}
}

func TestObservedReadinessUsesServiceAliasAndFriendlyID(t *testing.T) {
	const observedID = "mirrorbird-e2e-20261018.asse"
	const observedOrigin = "https://jqfmk0vr-43123.asse.devtunnels.ms"
	const stdout = "Connection to host tunnel relay restored.\nHosting port: 43123\nConnect via browser: " + observedOrigin + "\nInspect network activity: https://jqfmk0vr-43123-inspect.asse.devtunnels.ms\n\nReady to accept connections for tunnel: " + observedID + "\n"
	id, _ := parseID(observedID, true)
	output := newHostOutput(id, 43123)
	for _, ch := range []byte(stdout) {
		_, _ = output.Write([]byte{ch})
	}
	select {
	case result := <-output.ready:
		if result.err != nil || result.origin != observedOrigin {
			t.Fatalf("observed CLI stdout rejected: %+v", result)
		}
	default:
		t.Fatal("observed CLI stdout did not report readiness")
	}
	for _, unsafeOrigin := range []string{
		"https://jqfmk0vr-43123-inspect.asse.devtunnels.ms",
		"https://jqfmk0vr-43124.asse.devtunnels.ms",
		"https://jqfmk0vr-43123.usw2.devtunnels.ms",
		"https://jqfmk0vr-43123.asse.devtunnels.ms.evil.example",
		"https://user:secret@jqfmk0vr-43123.asse.devtunnels.ms",
		observedOrigin + "?token=secret",
	} {
		output := newHostOutput(id, 43123)
		text := strings.Replace(stdout, "Connect via browser: "+observedOrigin, "Connect via browser: "+unsafeOrigin, 1)
		_, _ = io.WriteString(output, text)
		select {
		case result := <-output.ready:
			if !errors.Is(result.err, ErrUnverified) || result.origin != "" {
				t.Errorf("accepted unsafe browser origin: %+v", result)
			}
		default:
			t.Error("unsafe browser origin was not rejected")
		}
	}
}

func TestReadinessIgnoresUnrelatedAndRejectsContradictoryOutput(t *testing.T) {
	id, _ := parseID(testID, true)
	for _, text := range []string{
		"started\nready\nlistening\n",
		"Hosting port: 43127\nConnect via browser: " + testOrigin + "/\n",
		"not really Ready to accept connections for tunnel: " + testID + "\n",
		"{\"url\":\"" + testOrigin + "\",\"ready\":true}\n",
	} {
		output := newHostOutput(id, testPort)
		_, _ = io.WriteString(output, text)
		select {
		case result := <-output.ready:
			t.Errorf("accepted no readiness %q: %+v", text, result)
		default:
		}
	}
	for _, text := range []string{
		strings.Replace(readyText, "Hosting port: 43127", "Hosting port: 22", 1),
		"Hosting port: 43127\n" + readyText,
		strings.Replace(readyText, "usw2.devtunnels.ms/", "usw2.devtunnels.ms.evil.example/", 1),
		strings.Replace(readyText, "Connect via browser: https:", "Connect via browser: http:", 1),
		strings.Replace(readyText, "tunnel: "+testID, "tunnel: other-resource.usw2", 1),
		"Ready to accept connections for tunnel: " + testID + "\n",
		"Connect via browser: " + testOrigin + "/\n" + readyText,
		strings.Replace(readyText, testOrigin+"/", testOrigin+"/, https://evil.example/", 1),
	} {
		output := newHostOutput(id, testPort)
		_, _ = io.WriteString(output, text)
		select {
		case result := <-output.ready:
			if !errors.Is(result.err, ErrUnverified) || result.origin != "" {
				t.Errorf("unsafe ready result %+v for %q", result, text)
			}
		default:
			t.Error("contradictory output did not reject startup")
		}
	}
}

func TestReadinessBoundsTotalAndSingleLineOutput(t *testing.T) {
	id, _ := parseID(testID, true)
	for _, text := range []string{strings.Repeat("x", 4097), strings.Repeat("trace\n", maxOutput/6+1)} {
		output := newHostOutput(id, testPort)
		if n, err := io.WriteString(output, text); n != len(text) || err != nil {
			t.Fatal("overflow must be drained")
		}
		select {
		case result := <-output.ready:
			if !errors.Is(result.err, ErrNotReady) {
				t.Fatalf("overflow err=%v", result.err)
			}
		default:
			t.Fatal("overflow failed to abort readiness")
		}
		if output.line != nil {
			t.Fatal("overflow retained output")
		}
	}
}

func TestLoginOutputHandlesFragmentsAndNotifiesOnce(t *testing.T) {
	for _, chunkSize := range []int{1, 2, 13, len(testLoginText)} {
		output := newLoginOutput()
		_, _ = io.WriteString(output, "unrelated secret diagnostic\n")
		for offset := 0; offset < len(testLoginText); offset += chunkSize {
			chunk := testLoginText[offset:min(offset+chunkSize, len(testLoginText))]
			if n, err := io.WriteString(output, chunk); n != len(chunk) || err != nil {
				t.Fatal("partial stdout write")
			}
		}
		select {
		case prompt := <-output.ready:
			if prompt.err != nil || prompt.challenge != (LoginChallenge{URL: testLoginURL, Code: testLoginCode}) {
				t.Fatal("did not emit the expected typed challenge")
			}
		default:
			t.Fatal("complete fragmented prompt was withheld")
		}
		_, _ = io.WriteString(output, testLoginText+strings.ReplaceAll(testLoginText, testLoginCode, "OTHER1234")+"secret trace")
		output.close()
		if output.line != nil {
			t.Fatal("retained raw output after challenge")
		}
		select {
		case <-output.ready:
			t.Fatal("notified more than once")
		default:
		}
	}
}

func TestLoginChallengeRejectsMalformedOrNonMicrosoftOutput(t *testing.T) {
	valid := strings.TrimSuffix(testLoginText, "\r\n")
	cases := []string{
		"Browse to https://github.com/login/device and enter code " + testLoginCode,
		"diagnostic: " + valid, valid + " extra secret", " " + valid, valid + " ",
		strings.TrimSuffix(valid, "."), strings.ToLower(valid),
		strings.Replace(valid, "To sign in,", "To sign in:", 1),
		"{\"message\":\"" + valid + "\"}", "\x1b[32m" + valid + "\x1b[0m",
	}
	for _, url := range []string{
		"http://login.microsoft.com/device", "https://login.microsoft.com/device?code=secret",
		"https://login.microsoft.com/device?", "https://login.microsoft.com/device#secret",
		"https://login.microsoft.com/device#", "https://login.microsoft.com/device/",
		"https://login.microsoft.com:443/device", "https://login.microsoft.com.evil.example/device",
		"https://evil.example/device", "https://login.microsoft.com@evil.example/device",
		"https://secret@login.microsoft.com/device", "https://login.microsoft.com./device",
		"https://login.microsoft.com/%64evice", "https://login.microsoft.com/other",
		"https://microsoft.com/device", "https://www.microsoft.com/devicelogin?token=secret",
		"https://microsoft.com/devicelogin#secret", "https://github.com/login/device",
		"file:///device", "javascript:alert(1)", "https://login.microsoft.com/device\x00",
	} {
		cases = append(cases, strings.Replace(valid, testLoginURL, url, 1))
	}
	for _, code := range []string{"", "ABCDE", strings.Repeat("A", 17), "EXAM-1234", "EXAM_1234", "EXAM 1234", "EXAM\t1234", "EXAM\n1234", "EXAM\x001234", "ＥXAMPLE12", "EXAMPLÉ12", "EXAMPLE12?token=secret"} {
		cases = append(cases, strings.Replace(valid, testLoginCode, code, 1))
	}
	for i, text := range cases {
		output := newLoginOutput()
		_, _ = io.WriteString(output, text+"\r\n")
		output.close()
		select {
		case <-output.ready:
			t.Errorf("accepted untrusted prompt fixture %d", i)
		default:
		}
		if output.line != nil {
			t.Fatal("kept raw output after exit")
		}
	}
}

func TestLoginOutputAcceptsOnlyExactMicrosoftPagesAndFinalLine(t *testing.T) {
	for _, url := range []string{testLoginURL, "https://microsoft.com/devicelogin", "https://www.microsoft.com/devicelogin"} {
		for _, code := range []string{"ABC123", testLoginCode, "abcdefghijkl1234"} {
			text := strings.ReplaceAll(strings.ReplaceAll(testLoginText, testLoginURL, url), testLoginCode, code)
			output := newLoginOutput()
			_, _ = io.WriteString(output, strings.TrimSuffix(text, "\r\n"))
			select {
			case <-output.ready:
				t.Fatal("accepted a line before its end was known")
			default:
			}
			output.close()
			select {
			case prompt := <-output.ready:
				if prompt.err != nil || prompt.challenge != (LoginChallenge{URL: url, Code: code}) {
					t.Fatal("wrong typed challenge")
				}
			default:
				t.Fatal("lost final complete sentence without newline")
			}
		}
	}
}

func TestLoginOutputBoundsLineAndTotalBytes(t *testing.T) {
	for _, singleLine := range []bool{false, true} {
		output := newLoginOutput()
		text := strings.Repeat("x", maxLoginLine+1)
		if !singleLine {
			text = strings.Repeat("trace\n", maxOutput/6+1)
		}
		for offset := 0; offset < len(text); offset += 100 {
			chunk := text[offset:min(offset+100, len(text))]
			if n, err := io.WriteString(output, chunk); n != len(chunk) || err != nil {
				t.Fatal("excess output was not drained")
			}
		}
		_, _ = io.WriteString(output, testLoginText)
		output.close()
		select {
		case prompt := <-output.ready:
			if !errors.Is(prompt.err, ErrLoginNoChallenge) || prompt.challenge != (LoginChallenge{}) {
				t.Fatal("overflow did not fail closed")
			}
		default:
			t.Fatal("overflow did not interrupt login")
		}
		if output.line != nil || output.total > maxOutput {
			t.Fatal("overflow retained raw output or exceeded bounds")
		}
		select {
		case <-output.ready:
			t.Fatal("emitted a challenge after overflow")
		default:
		}
	}
}
