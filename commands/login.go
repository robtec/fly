package commands

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/concourse/atc"
	"github.com/concourse/fly/rc"
	"github.com/concourse/go-concourse/concourse"
)

type LoginCommand struct {
	ATCURL   string       `short:"c" long:"concourse-url" description:"Concourse URL to authenticate with"`
	Insecure bool         `short:"k" long:"insecure" description:"Skip verification of the endpoint's SSL certificate"`
	Username string       `short:"u" long:"username" description:"Username for basic auth"`
	Password string       `short:"p" long:"password" description:"Password for basic auth"`
	TeamName string       `short:"n" long:"team-name" description:"Team to authenticate with"`
	CACert   atc.PathFlag `long:"ca-cert" description:"Path to Concourse PEM-encoded CA certificate file."`
}

func (command *LoginCommand) Execute(args []string) error {
	if Fly.Target == "" {
		return errors.New("name for the target must be specified (--target/-t)")
	}

	var target rc.Target
	var err error

	var caCert string
	if command.CACert != "" {
		caCertBytes, err := ioutil.ReadFile(string(command.CACert))
		if err != nil {
			return err
		}
		caCert = string(caCertBytes)
	}

	if command.ATCURL != "" {
		if command.TeamName == "" {
			command.TeamName = atc.DefaultTeamName
		}

		target, err = rc.NewUnauthenticatedTarget(
			Fly.Target,
			command.ATCURL,
			command.TeamName,
			command.Insecure,
			caCert,
			Fly.Verbose,
		)
	} else {
		target, err = rc.LoadTargetWithInsecure(
			Fly.Target,
			command.TeamName,
			command.Insecure,
			caCert,
			Fly.Verbose,
		)
	}
	if err != nil {
		return err
	}

	client := target.Client()
	command.TeamName = target.Team().Name()

	fmt.Printf("logging in to team '%s'\n\n", command.TeamName)

	if len(args) != 0 {
		return errors.New("unexpected argument [" + strings.Join(args, ", ") + "]")
	}

	err = target.ValidateWithWarningOnly()
	if err != nil {
		return err
	}

	var tokenType string
	var tokenValue string

	if command.Username != "" && command.Password != "" {
		tokenType, tokenValue, err = command.passwordGrant(client, command.Username, command.Password)
	} else {
		tokenType, tokenValue, err = command.authCodeGrant(client.URL())
	}
	if err != nil {
		return err
	}

	fmt.Println("")

	return command.saveTarget(
		client.URL(),
		&rc.TargetToken{
			Type:  tokenType,
			Value: tokenValue,
		},
		target.CACert(),
	)
}

func (command *LoginCommand) passwordGrant(client concourse.Client, username, password string) (string, string, error) {

	token, err := client.PasswordGrant(username, password)
	if err != nil {
		return "", "", err
	}

	return token.TokenType, token.AccessToken, nil
}

func (command *LoginCommand) authCodeGrant(targetUrl string) (string, string, error) {

	var tokenStr string

	stdinChannel := make(chan string)
	tokenChannel := make(chan string)
	errorChannel := make(chan error)
	portChannel := make(chan string)

	go listenForTokenCallback(tokenChannel, errorChannel, portChannel, targetUrl)

	port := <-portChannel

	redirectUrl, err := url.Parse("http://127.0.0.1:" + port + "/auth/callback")
	if err != nil {
		panic(err)
	}

	fmt.Println("navigate to the following URL in your browser:")
	fmt.Println("")
	fmt.Printf("    %s/sky/login?redirect=%s\n", targetUrl, redirectUrl.String())
	fmt.Println("")

	go waitForTokenInput(stdinChannel, errorChannel)

	select {
	case tokenStrMsg := <-tokenChannel:
		tokenStr = tokenStrMsg
	case tokenStrMsg := <-stdinChannel:
		tokenStr = tokenStrMsg
	case errorMsg := <-errorChannel:
		return "", "", errorMsg
	}

	segments := strings.SplitN(tokenStr, " ", 2)

	return segments[0], segments[1], nil
}

func listenForTokenCallback(tokenChannel chan string, errorChannel chan error, portChannel chan string, targetUrl string) {
	s := &http.Server{
		Addr: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenChannel <- r.FormValue("token")
			http.Redirect(w, r, fmt.Sprintf("%s/public/fly_success", targetUrl), http.StatusTemporaryRedirect)
		}),
	}

	err := listenAndServeWithPort(s, portChannel)

	if err != nil {
		errorChannel <- err
	}
}

func listenAndServeWithPort(srv *http.Server, portChannel chan string) error {
	addr := srv.Addr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return err
	}

	portChannel <- port

	return srv.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}

type tcpKeepAliveListener struct {
	*net.TCPListener
}

func waitForTokenInput(tokenChannel chan string, errorChannel chan error) {
	for {
		fmt.Printf("or enter token manually: ")

		var tokenType string
		var tokenValue string
		count, err := fmt.Scanf("%s %s", &tokenType, &tokenValue)
		if err != nil {
			if count != 2 {
				fmt.Println("token must be of the format 'TYPE VALUE', e.g. 'Bearer ...'")
				continue
			}

			errorChannel <- err
			return
		}

		tokenChannel <- tokenType + " " + tokenValue
		break
	}
}

func (command *LoginCommand) saveTarget(url string, token *rc.TargetToken, caCert string) error {
	err := rc.SaveTarget(
		Fly.Target,
		url,
		command.Insecure,
		command.TeamName,
		&rc.TargetToken{
			Type:  token.Type,
			Value: token.Value,
		},
		caCert,
	)
	if err != nil {
		return err
	}

	fmt.Println("target saved")

	return nil
}
