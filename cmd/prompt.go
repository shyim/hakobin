package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func valueOrPrompt(value string, promptText string, defaultValue string) (string, error) {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	fmt.Printf("%s [%s]: ", promptText, defaultValue)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			return defaultValue, nil
		}
		return text, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return defaultValue, nil
}

func listOrPrompt(value []string, promptText string, defaultValue string) ([]string, error) {
	if len(value) > 0 {
		return value, nil
	}
	raw, err := valueOrPrompt("", promptText, defaultValue)
	if err != nil {
		return nil, err
	}
	var res []string
	for _, v := range strings.Split(raw, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			res = append(res, v)
		}
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("%s must contain at least one value", promptText)
	}
	return res, nil
}
