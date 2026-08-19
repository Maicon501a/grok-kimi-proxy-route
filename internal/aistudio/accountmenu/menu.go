package accountmenu

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"grok-desktop/internal/aistudio/cdp"
	"grok-desktop/internal/aistudio/config"
	"grok-desktop/internal/aistudio/profile"
	runtimepkg "grok-desktop/internal/aistudio/runtime"
	"grok-desktop/internal/aistudio/storage"
)

type Menu struct {
	runtime      *runtimepkg.Manager
	profiles     *profile.Registry
	cfg          *config.Config
	accountsRoot string
	manualMu     sync.Mutex
	manualLogins map[string]*manualLoginSession
}

type manualLoginSession struct {
	profile  profile.Profile
	original profile.Profile
	client   *cdp.Client
	created  bool
}

type Result struct {
	Action string
}

type validationResult struct {
	OK     bool
	Email  string
	Reason string
}

func New(mgr *runtimepkg.Manager) *Menu {
	return &Menu{
		runtime:      mgr,
		profiles:     mgr.Profiles(),
		cfg:          mgr.Config(),
		accountsRoot: filepath.Join(mgr.Config().StateDir, "accounts"),
		manualLogins: make(map[string]*manualLoginSession),
	}
}

func (m *Menu) RunInteractiveMenu() (*Result, error) {
	if !isInteractiveTerminal() {
		return &Result{Action: "start"}, nil
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		m.renderMenu()
		choice, err := ask(reader, "Select an option [Enter=start]: ")
		if err != nil {
			return nil, err
		}
		switch strings.ToUpper(strings.TrimSpace(choice)) {
		case "", "S":
			return &Result{Action: "start"}, nil
		case "Q":
			return &Result{Action: "quit"}, nil
		case "M":
			if err := m.addAccountManual(reader); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		case "O":
			if err := m.logoutAccount(reader); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		case "R":
			if err := m.removeAccount(reader); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		case "L":
			if err := m.loginAllAccounts(reader); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		case "A":
			if _, _, err := m.AutoValidateAllAccounts(); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		case "B":
			if err := m.configureBrowserMode(reader); err != nil {
				fmt.Printf("[AccountManager] Erro: %v\n", err)
			}
		default:
			fmt.Println("[AccountManager] Opcao invalida.")
		}
	}
}

func (m *Menu) AutoValidateAllAccounts() (validCount, invalidCount int, err error) {
	profiles := m.profiles.List()
	if len(profiles) == 0 {
		fmt.Println("[AccountManager] Nenhuma conta configurada.")
		return 0, 0, nil
	}

	fmt.Println()
	fmt.Println("=== Iniciando Validacao Automatica de Contas (Headless) ===")
	for _, p := range profiles {
		label := firstNonEmpty(p.Email, p.Label, p.ID)
		fmt.Printf("[AccountManager] Validando %s...\n", label)

		result, validateErr := m.validateAccountAuto(p)
		if validateErr != nil {
			result = &validationResult{OK: false, Reason: validateErr.Error()}
		}

		if result.OK {
			next := p
			next.Email = firstNonEmpty(result.Email, p.Email)
			next.Label = firstNonEmpty(result.Email, p.Label, p.ID)
			next.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
			next.LoginMode = "manual_browser"
			next.ValidationError = ""
			valid := true
			next.IsValid = &valid
			if err := m.persistProfile(next); err != nil {
				return validCount, invalidCount, err
			}
			fmt.Printf("[AccountManager] [OK] Conta ativa e validada: %s\n", firstNonEmpty(result.Email, p.ID))
			validCount++
			continue
		}

		next := p
		valid := false
		next.IsValid = &valid
		next.ValidationError = firstNonEmpty(result.Reason, "erro desconhecido")
		if err := m.persistProfile(next); err != nil {
			return validCount, invalidCount, err
		}
		fmt.Printf("[AccountManager] [FALHA] Conta deslogada ou invalida (%s): %s\n", label, next.ValidationError)
		invalidCount++
	}

	fmt.Println()
	fmt.Println("=== Resumo da Validacao Automatica ===")
	fmt.Printf("Contas OK: %d\n", validCount)
	fmt.Printf("Contas com Falha: %d\n", invalidCount)
	if invalidCount > 0 {
		fmt.Println("[AccountManager] Nota: Use a opcao manual [L] para re-autenticar as contas com falha.")
	}
	fmt.Println("=========================================================")
	fmt.Println()
	return validCount, invalidCount, nil
}

func (m *Menu) renderMenu() {
	profiles := m.runtime.ListProfiles()
	summaries := m.runtime.Accounts().Summarize(profileIDs(profiles), m.runtime.Sessions().CountByProfile())
	summaryByID := map[string]bool{}
	sessionByID := map[string]int{}
	for _, summary := range summaries {
		summaryByID[summary.ProfileID] = summary.Available
		sessionByID[summary.ProfileID] = summary.ActiveSessions
	}

	fmt.Println()
	fmt.Println("=== Proxy AI Studio Account Manager ===")
	fmt.Println()
	fmt.Printf("Browser mode: %s%s\n", describeBrowserMode(m.cfg), recommendedBrowserSuffix(m.cfg))
	fmt.Println()
	fmt.Printf("Configured accounts (%d):\n", len(profiles))
	fmt.Println()

	defaultID := m.profiles.DefaultID()
	for i, p := range profiles {
		title := firstNonEmpty(p.Email, p.Label, p.ID)
		availability := "ready"
		if !summaryByID[p.ID] {
			availability = "cooldown"
		}
		if p.IsValid != nil && !*p.IsValid {
			availability = "invalid/needs-login"
		}
		defaultSuffix := ""
		if p.ID == defaultID {
			defaultSuffix = " [default]"
		}
		fmt.Printf(" [%d] %s%s (ID: %s, sessions: %d, status: %s)\n",
			i+1, title, defaultSuffix, p.ID, sessionByID[p.ID], availability)
	}
	if len(profiles) == 0 {
		fmt.Println(" Nenhuma conta configurada.")
	}

	fmt.Println()
	fmt.Println("Options:")
	fmt.Println(" [S] Start server")
	fmt.Println(" [M] Add account (manual browser login)")
	fmt.Println(" [O] Logout account")
	fmt.Println(" [R] Remove account")
	fmt.Println(" [L] Login/check all accounts (manual browser)")
	fmt.Println(" [A] Auto-validate all accounts (headless check)")
	fmt.Println(" [B] Browser mode")
	fmt.Println(" [Q] Quit")
	fmt.Println()
}

func (m *Menu) configureBrowserMode(reader *bufio.Reader) error {
	fmt.Println()
	fmt.Println("Browser modes:")
	fmt.Println(" [1] Headless + spoof (Recommended, default for chat)")
	fmt.Println(" [2] Visible legacy (Deprecated)")
	fmt.Println()

	raw, err := ask(reader, "Select mode [Enter=cancel]: ")
	if err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		fmt.Println("[AccountManager] Alteracao cancelada.")
		return nil
	}

	var next config.BrowserMode
	switch raw {
	case "1":
		next = config.BrowserHeadlessSpoof
	case "2":
		next = config.BrowserVisibleLegacy
	default:
		fmt.Println("[AccountManager] Modo invalido.")
		return nil
	}

	if m.cfg.AIStudio.BrowserMode == next {
		fmt.Println("[AccountManager] Esse modo ja esta ativo.")
		return nil
	}

	if err := persistBrowserMode(m.cfg.AIStudio.BrowserModeFile, next); err != nil {
		return err
	}
	applyBrowserMode(m.cfg, next)
	m.runtime.ShutdownAllManagedBrowsers(context.Background())
	fmt.Printf("[AccountManager] Browser mode atualizado para: %s\n", describeBrowserMode(m.cfg))
	fmt.Println("[AccountManager] Browsers gerenciados foram encerrados; o novo modo sera usado no proximo launch.")
	return nil
}

func (m *Menu) addAccountManual(reader *bufio.Reader) error {
	current := m.listVisibleProfiles(false)
	p := m.buildNewProfile(len(current))
	fmt.Printf("[AccountManager] Abrindo navegador novo para %s...\n", p.ID)

	result, err := m.runManualLoginFlow(p, reader, "Entre no AI Studio nessa janela. Quando terminar, volte aqui e pressione Enter.")
	if err != nil {
		cleanupProfileArtifacts(p)
		return err
	}
	if result == nil {
		cleanupProfileArtifacts(p)
		fmt.Println("[AccountManager] Cadastro cancelado.")
		return nil
	}

	p.Email = firstNonEmpty(result.Email, p.Email)
	p.Label = firstNonEmpty(result.Email, p.Label, p.ID)
	p.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
	p.LoginMode = "manual_browser"
	p.ValidationError = ""
	valid := true
	p.IsValid = &valid
	if err := m.persistProfile(p); err != nil {
		return err
	}
	fmt.Printf("[AccountManager] Conta salva: %s\n", firstNonEmpty(p.Email, p.ID))
	m.warmUpProfile(p.ID)
	return nil
}

func (m *Menu) logoutAccount(reader *bufio.Reader) error {
	p, err := m.pickProfile(reader, "Numero da conta para logout: ")
	if err != nil || p == nil {
		return err
	}
	m.runtime.DisconnectProfile(p.ID)
	cleanupProfileArtifacts(*p)

	next := *p
	next.Email = ""
	next.LastLoginAt = ""
	next.Label = fmt.Sprintf("Conta (%s)", p.ID)
	next.ValidationError = ""
	if err := m.persistProfile(next); err != nil {
		return err
	}
	fmt.Printf("[AccountManager] Logout/local reset concluido para %s.\n", p.ID)
	return nil
}

func (m *Menu) removeAccount(reader *bufio.Reader) error {
	p, err := m.pickProfile(reader, "Numero da conta para remover: ")
	if err != nil || p == nil {
		return err
	}
	confirm, err := ask(reader, fmt.Sprintf("Confirma remover %s? [y/N]: ", firstNonEmpty(p.Email, p.ID)))
	if err != nil {
		return err
	}
	confirm = strings.ToLower(strings.TrimSpace(confirm))
	if confirm != "y" && confirm != "s" {
		fmt.Println("[AccountManager] Remocao cancelada.")
		return nil
	}

	m.runtime.DisconnectProfile(p.ID)
	m.runtime.Sessions().ClearProfileBindings(p.ID, "profile_removed")
	if err := m.profiles.RemoveProfile(p.ID); err != nil {
		return err
	}
	cleanupProfileArtifacts(*p)
	fmt.Printf("[AccountManager] Conta removida: %s.\n", p.ID)
	return nil
}

func (m *Menu) loginAllAccounts(reader *bufio.Reader) error {
	profiles := m.profiles.List()
	if len(profiles) == 0 {
		fmt.Println("[AccountManager] Nenhuma conta configurada.")
		return nil
	}

	for _, p := range profiles {
		fmt.Printf("[AccountManager] Verificando %s...\n", firstNonEmpty(p.Email, p.ID))
		result, err := m.runManualLoginFlow(p, reader, "Se a conta ja estiver logada, basta pressionar Enter para validar.")
		if err != nil {
			return err
		}
		if result == nil {
			fmt.Printf("[AccountManager] Conta ignorada: %s\n", p.ID)
			continue
		}

		next := p
		next.Email = firstNonEmpty(result.Email, p.Email)
		next.Label = firstNonEmpty(result.Email, p.Label, p.ID)
		next.LastLoginAt = time.Now().UTC().Format(time.RFC3339)
		next.LoginMode = "manual_browser"
		next.ValidationError = ""
		valid := true
		next.IsValid = &valid
		if err := m.persistProfile(next); err != nil {
			return err
		}
		fmt.Printf("[AccountManager] Conta pronta: %s\n", firstNonEmpty(result.Email, p.ID))
		m.warmUpProfile(p.ID)
	}
	return nil
}

func (m *Menu) validateAccountAuto(p profile.Profile) (*validationResult, error) {
	return m.validateAccountAutoContext(context.Background(), p)
}

func (m *Menu) validateAccountAutoContext(parent context.Context, p profile.Profile) (*validationResult, error) {
	cfg := cloneConfigWithBrowserMode(m.cfg, config.BrowserHeadlessSpoof)
	client := cdp.New(&p, cfg)
	defer client.Disconnect()

	if err := client.RunExclusive(parent, "account-validate-auto", func(ctx context.Context) error {
		return chromedp.Run(ctx,
			chromedp.Navigate(cfg.AIStudio.URL),
			chromedp.Sleep(1500*time.Millisecond),
		)
	}); err != nil {
		return &validationResult{OK: false, Reason: err.Error()}, nil
	}
	return validateLoggedInSession(client)
}

func (m *Menu) runManualLoginFlow(p profile.Profile, reader *bufio.Reader, title string) (*validationResult, error) {
	cfg := cloneConfigWithBrowserMode(m.cfg, config.BrowserVisibleLegacy)
	client := cdp.New(&p, cfg)
	defer client.Disconnect()

	if err := client.RunExclusive(context.Background(), "manual-login-open", func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Navigate(cfg.AIStudio.URL))
	}); err != nil {
		return nil, err
	}

	fmt.Println()
	fmt.Printf("[AccountManager] %s\n", title)
	fmt.Printf("[AccountManager] Perfil salvo em: %s\n", p.UserDataDir)

	for {
		input, err := ask(reader, "Pressione Enter para validar ou digite C para cancelar: ")
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(input), "c") {
			return nil, nil
		}

		result, err := validateLoggedInSession(client)
		if err != nil {
			return nil, err
		}
		if result.OK {
			return result, nil
		}
		fmt.Printf("[AccountManager] Ainda nao detectei login valido: %s\n", result.Reason)
	}
}

func validateLoggedInSession(client *cdp.Client) (*validationResult, error) {
	return validateLoggedInSessionContext(context.Background(), client)
}

func validateLoggedInSessionContext(parent context.Context, client *cdp.Client) (*validationResult, error) {
	result := &validationResult{}
	err := client.RunExclusive(parent, "validate-logged-in-session", func(ctx context.Context) error {
		cookies, err := client.GetCookies(ctx)
		if err != nil {
			return err
		}
		hasAuthCookie := false
		for _, cookie := range cookies {
			if cookie.Name == "SAPISID" || cookie.Name == "__Secure-3PAPISID" {
				hasAuthCookie = true
				break
			}
		}
		if !hasAuthCookie {
			result.OK = false
			result.Reason = "cookie SAPISID nao encontrado"
			return nil
		}

		email, err := detectAccountEmail(client, ctx)
		if err != nil {
			return err
		}
		result.OK = true
		result.Email = email
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func detectAccountEmail(client *cdp.Client, ctx context.Context) (string, error) {
	emailExpr := `(function () {
		const textSamples = new Set();
		const selectors = ['body', '[aria-label]', '[title]', 'img[alt]'];
		for (const selector of selectors) {
			const nodes = document.querySelectorAll(selector);
			for (const node of nodes) {
				const values = [];
				if (node.textContent) values.push(node.textContent);
				if (node.getAttribute) {
					values.push(node.getAttribute('aria-label'));
					values.push(node.getAttribute('title'));
					values.push(node.getAttribute('alt'));
				}
				for (const value of values) {
					if (value) textSamples.add(value);
				}
			}
		}
		const regex = /[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/ig;
		for (const sample of textSamples) {
			const match = sample.match(regex);
			if (match && match[0]) return match[0].toLowerCase();
		}
		return null;
	})()`

	var current string
	if err := client.Evaluate(ctx, emailExpr, &current); err == nil && strings.TrimSpace(current) != "" {
		return strings.TrimSpace(current), nil
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://myaccount.google.com/"),
		chromedp.Sleep(1500*time.Millisecond),
	); err != nil {
		return "", nil
	}

	var fallback string
	fallbackExpr := `(function () {
		const text = document.body ? document.body.innerText : '';
		const match = text.match(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/i);
		return match && match[0] ? match[0].toLowerCase() : null;
	})()`
	if err := client.Evaluate(ctx, fallbackExpr, &fallback); err != nil {
		return "", nil
	}
	return strings.TrimSpace(fallback), nil
}

func (m *Menu) warmUpProfile(profileID string) {
	rt := m.runtime.GetRuntimeIfExists(profileID)
	if rt == nil || rt.Chat == nil {
		return
	}
	result, err := rt.Chat.WarmUpHttpCaches(context.Background())
	if err != nil {
		fmt.Printf("[AccountManager] Warmup error for %s: %v\n", profileID, err)
		return
	}
	if result.Warmed {
		fmt.Printf("[AccountManager] HTTP cache %s: ready\n", profileID)
		return
	}
	fmt.Printf("[AccountManager] HTTP cache %s: skipped (%s)\n", profileID, result.Reason)
}

func (m *Menu) buildNewProfile(existingCount int) profile.Profile {
	id := "account-" + shortRandomID()
	accountDir := filepath.Join(m.accountsRoot, id)
	return profile.Profile{
		ID:             id,
		Label:          fmt.Sprintf("Conta %d", existingCount+1),
		ConnectionFile: filepath.Join(accountDir, "connection.json"),
		UserDataDir:    filepath.Join(accountDir, "user-data"),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		LoginMode:      "manual_browser",
	}
}

func (m *Menu) persistProfile(next profile.Profile) error {
	existing := m.listVisibleProfiles(false)
	profiles := make([]profile.Profile, 0, len(existing)+1)
	for _, current := range existing {
		if current.ID != next.ID {
			profiles = append(profiles, current)
		}
	}
	profiles = append(profiles, next)

	defaultProfileID := m.profiles.DefaultID()
	if len(existing) == 0 {
		defaultProfileID = next.ID
	}
	if strings.TrimSpace(defaultProfileID) == "" {
		defaultProfileID = m.cfg.Profiles.DefaultID
	}
	return m.profiles.SaveProfiles(profiles, defaultProfileID)
}

func (m *Menu) listVisibleProfiles(includePlaceholderFallback bool) []profile.Profile {
	profiles := m.profiles.List()
	if includePlaceholderFallback {
		return profiles
	}
	out := make([]profile.Profile, 0, len(profiles))
	for _, p := range profiles {
		if !m.isPlaceholderFallbackProfile(p) {
			out = append(out, p)
		}
	}
	return out
}

func (m *Menu) isPlaceholderFallbackProfile(p profile.Profile) bool {
	if p.ID != m.cfg.Profiles.DefaultID {
		return false
	}
	if p.Email != "" || p.LastLoginAt != "" || p.LoginMode != "" {
		return false
	}
	if p.ConnectionFile != "" {
		if _, err := os.Stat(p.ConnectionFile); err == nil {
			return false
		}
	}
	return m.profiles.IsUsingFallback()
}

func (m *Menu) pickProfile(reader *bufio.Reader, prompt string) (*profile.Profile, error) {
	profiles := m.profiles.List()
	if len(profiles) == 0 {
		fmt.Println("[AccountManager] Nenhuma conta configurada.")
		return nil, nil
	}
	raw, err := ask(reader, prompt)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	index := 0
	_, err = fmt.Sscanf(raw, "%d", &index)
	if err != nil || index < 1 || index > len(profiles) {
		fmt.Println("[AccountManager] Conta invalida.")
		return nil, nil
	}
	selected := profiles[index-1]
	return &selected, nil
}

func persistBrowserMode(path string, mode config.BrowserMode) error {
	encoded, err := json.MarshalIndent(map[string]any{
		"mode":      string(mode),
		"updatedAt": time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	return storage.WriteAtomic(path, encoded)
}

func applyBrowserMode(cfg *config.Config, mode config.BrowserMode) {
	cfg.AIStudio.BrowserMode = mode
	cfg.AIStudio.ManagedHeadless = mode != config.BrowserVisibleLegacy
	cfg.AIStudio.ManagedHeadlessSpoofVisible = mode == config.BrowserHeadlessSpoof
}

func describeBrowserMode(cfg *config.Config) string {
	switch cfg.AIStudio.BrowserMode {
	case config.BrowserVisibleLegacy:
		return "Visible legacy"
	case config.BrowserHeadlessRaw:
		return "Headless raw"
	default:
		return "Headless + spoof"
	}
}

func recommendedBrowserSuffix(cfg *config.Config) string {
	if cfg.AIStudio.BrowserMode == config.BrowserHeadlessSpoof {
		return " (Recommended)"
	}
	return ""
}

func ask(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func profileIDs(profiles []profile.Profile) []string {
	out := make([]string, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.ID)
	}
	return out
}

func cleanupProfileArtifacts(p profile.Profile) {
	if p.ConnectionFile != "" {
		_ = os.Remove(p.ConnectionFile)
	}
	if p.UserDataDir != "" {
		_ = os.RemoveAll(p.UserDataDir)
	}
	parentDir := ""
	if p.ConnectionFile != "" {
		parentDir = filepath.Dir(p.ConnectionFile)
	}
	if parentDir != "" {
		entries, err := os.ReadDir(parentDir)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(parentDir)
		}
	}
}

func cloneConfigWithBrowserMode(src *config.Config, mode config.BrowserMode) *config.Config {
	next := *src
	next.AIStudio = src.AIStudio
	applyBrowserMode(&next, mode)
	return &next
}

func shortRandomID() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isInteractiveTerminal() bool {
	in, err := os.Stdin.Stat()
	if err != nil || (in.Mode()&os.ModeCharDevice) == 0 {
		return false
	}
	out, err := os.Stdout.Stat()
	if err != nil || (out.Mode()&os.ModeCharDevice) == 0 {
		return false
	}
	return true
}
