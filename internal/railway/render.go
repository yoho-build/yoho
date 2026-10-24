package railway

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func render(inv inventory, source string, extra []string) (*Result, error) {
	allowNoImage := strings.HasPrefix(source, "config-as-code")
	if err := prepare(&inv, allowNoImage); err != nil {
		return nil, err
	}
	d := &Result{Warnings: append([]string{}, extra...)}
	seenVol := map[string]string{}
	var wget bool
	for i := range inv.Services {
		s := &inv.Services[i]
		d.Services = append(d.Services, s.Name)
		if s.Cron != "" {
			d.Warnings = append(d.Warnings, s.Name+": Railway cron ("+s.Cron+") is not a Yoho Scheduled Job. Scheduled Jobs are Backups. Run the command as a Service.")
		}
		s.Env, s.SecretKeys = dropRailway(d, s.Name, s.Env, s.SecretKeys)
		if isPostgres(s.Image) {
			if s.Env == nil {
				s.Env = map[string]string{}
			}
			if _, ok := s.Env["POSTGRES_USER"]; !ok {
				s.Env["POSTGRES_USER"] = "postgres"
			}
			if _, ok := s.Env["POSTGRES_DB"]; !ok {
				s.Env["POSTGRES_DB"] = "postgres"
			}
			if err := ident("POSTGRES_USER", s.Env["POSTGRES_USER"]); err != nil {
				return nil, err
			}
			if err := ident("POSTGRES_DB", s.Env["POSTGRES_DB"]); err != nil {
				return nil, err
			}
			if !contains(s.SecretKeys, "POSTGRES_PASSWORD") {
				if contains(d.Generated, "POSTGRES_PASSWORD") {
					return nil, &InputError{Message: "only one Postgres Service can generate POSTGRES_PASSWORD", Hint: "use one database Service, or put POSTGRES_PASSWORD in secret_keys"}
				}
				d.Generated = append(d.Generated, "POSTGRES_PASSWORD")
			}
		}
		if s.Volume != "" {
			if prev, ok := seenVol[s.Volume]; ok {
				return nil, &InputError{Message: "volume " + s.Volume + " is used by " + prev + " and " + s.Name, Hint: "give each Service its own volume; two Postgres servers must not share one"}
			}
			seenVol[s.Volume] = s.Name
			d.Volumes = append(d.Volumes, s.Volume)
		}
		if s.HealthcheckPath != "" && len(s.Healthcheck) == 0 && s.Port > 0 {
			wget = true
		}
		for _, dep := range s.DependsOn {
			if isPostgres(serviceByName(inv, dep).Image) && !contains(s.SecretKeys, "POSTGRES_PASSWORD") && contains(d.Generated, "POSTGRES_PASSWORD") {
				d.Warnings = append(d.Warnings, s.Name+": does not list POSTGRES_PASSWORD, so it will not receive the generated database password")
			}
		}
	}
	if wget {
		d.Warnings = append([]string{"HTTP healthchecks use wget. The image must include wget; change the test to curl if it does not."}, d.Warnings...)
	}
	d.Compose = renderCompose(source, inv, d.Warnings)
	d.Yoho, d.SecretLines, d.SecretKeys = renderYoho(source, inv, d)
	return d, nil
}

func prepare(inv *inventory, allowNoImage bool) error {
	if !namePattern.MatchString(inv.Project) {
		return &InputError{Message: "project " + quote(inv.Project) + " must match " + namePattern.String(), Hint: "use a short lowercase name; it becomes the Yoho app name"}
	}
	if len(inv.Services) == 0 {
		return &InputError{Message: "services is empty", Hint: "list each Railway service (web, worker, Postgres)"}
	}
	if len(inv.Services) > 50 {
		return &InputError{Message: "too many services", Hint: "draft one project at a time"}
	}
	for _, h := range inv.Hostnames {
		if !hostPattern.MatchString(h) || len(h) > 253 {
			return &InputError{Message: "hostname " + quote(h) + " is not a DNS name", Hint: "use a name such as shop.example.com, without a scheme or path"}
		}
	}
	names := map[string]bool{}
	public := 0
	for i := range inv.Services {
		s := &inv.Services[i]
		if !namePattern.MatchString(s.Name) {
			return &InputError{Message: "service name " + quote(s.Name) + " must match " + namePattern.String(), Hint: "rename it to lowercase letters, digits, and hyphens (Railway's display name can differ)"}
		}
		if names[s.Name] {
			return &InputError{Message: "duplicate service " + s.Name, Hint: "each compose Service name is used once"}
		}
		names[s.Name] = true
		if s.Image != "" && s.Dockerfile != "" {
			return &InputError{Message: s.Name + " sets both image and dockerfile", Hint: "use the image for Postgres and a Dockerfile for the app you build"}
		}
		if s.Image == "" && s.Dockerfile == "" && !allowNoImage {
			return &InputError{Message: s.Name + " needs image or dockerfile", Hint: "Postgres is an image such as postgres:17-alpine; the app is a Dockerfile"}
		}
		if s.Dockerfile != "" {
			if path.IsAbs(s.Dockerfile) || strings.Contains(s.Dockerfile, "..") {
				return &InputError{Message: s.Name + ": dockerfile must be a relative path inside the build context", Hint: "use Dockerfile or a path such as docker/Dockerfile"}
			}
		}
		if len(s.Command) > 0 && s.StartCommand != "" {
			return &InputError{Message: s.Name + " sets both command and start_command", Hint: "use command for argv, or start_command for Railway's single startCommand string"}
		}
		if len(s.ReleaseCommand) > 0 && s.PreDeployCommand != "" {
			return &InputError{Message: s.Name + " sets both release_command and pre_deploy_command", Hint: "use release_command for argv, or pre_deploy_command for Railway's preDeployCommand string"}
		}
		if err := noNewlines(s.Name+" start_command", s.StartCommand); err != nil {
			return err
		}
		if err := noNewlines(s.Name+" pre_deploy_command", s.PreDeployCommand); err != nil {
			return err
		}
		for _, c := range append(append([]string{}, s.Command...), s.ReleaseCommand...) {
			if err := noNewlines(s.Name+" command", c); err != nil {
				return err
			}
		}
		if s.Port < 0 || s.Port > 65535 {
			return &InputError{Message: fmt.Sprintf("%s: port %d is out of range", s.Name, s.Port), Hint: "use the container port Railway routes to"}
		}
		if len(s.Hosts) > 0 {
			s.Public = true
		}
		if s.Public {
			public++
		}
		if s.Public && s.Port == 0 {
			return &InputError{Message: s.Name + " is public but has no port", Hint: "set port to the container port"}
		}
		if s.HealthcheckPath != "" && len(s.Healthcheck) > 0 {
			return &InputError{Message: s.Name + " sets both healthcheck_path and healthcheck", Hint: "use healthcheck_path for an HTTP path, or healthcheck for a compose CMD argv"}
		}
		if s.HealthcheckPath != "" && !strings.HasPrefix(s.HealthcheckPath, "/") {
			return &InputError{Message: s.Name + ": healthcheck_path must start with /", Hint: "Railway's healthcheckPath is a path such as /up"}
		}
		if strings.ContainsAny(s.HealthcheckPath, " \t\r\n") {
			return &InputError{Message: s.Name + ": healthcheck_path must be a single path", Hint: "use a path such as /up"}
		}
		if _, err := restartPolicy(*s); err != nil {
			return err
		}
		if s.VolumeMount != "" {
			if !pathPattern.MatchString(s.VolumeMount) || strings.Contains(s.VolumeMount, "..") {
				return &InputError{Message: s.Name + ": volume_mount must be an absolute container path", Hint: "use the mount path from Railway, such as /var/lib/postgresql/data"}
			}
		}
		if isPostgres(s.Image) {
			if s.VolumeMount == "" {
				s.VolumeMount = "/var/lib/postgresql/data"
			}
			if s.Volume == "" {
				s.Volume = "pgdata"
			}
		} else if s.VolumeMount != "" && s.Volume == "" {
			s.Volume = s.Name + "-data"
		}
		if s.Volume != "" && !namePattern.MatchString(s.Volume) {
			return &InputError{Message: s.Name + ": volume " + quote(s.Volume) + " must match " + namePattern.String(), Hint: "use a short name such as pgdata"}
		}
		for k, v := range s.Env {
			if !keyPattern.MatchString(k) {
				return &InputError{Message: s.Name + ": env key " + quote(k) + " is not a shell name", Hint: "use names like POSTGRES_USER"}
			}
			if sensitiveKey(strings.ToLower(k)) {
				return &InputError{Message: s.Name + ": env." + k + " looks like a secret", Hint: "move the name to secret_keys. Do not put the value in the inventory"}
			}
			if strings.TrimSpace(v) == "" {
				return &InputError{Message: s.Name + ": env." + k + " is empty", Hint: "set the non-secret literal, or drop the key"}
			}
			if err := noNewlines(s.Name+" env."+k, v); err != nil {
				return err
			}
		}
		for _, k := range s.SecretKeys {
			if strings.Contains(k, "=") {
				return &InputError{Message: s.Name + ": secret_keys must be names, not KEY=value", Hint: "put the value in 1Password or Bitwarden and keep only the name here"}
			}
			if !keyPattern.MatchString(k) && !strings.HasPrefix(k, "RAILWAY_") {
				return &InputError{Message: s.Name + ": secret key " + quote(k) + " is not a shell name", Hint: "use the variable name only"}
			}
		}
		for _, h := range s.Hosts {
			if !hostPattern.MatchString(h) || len(h) > 253 {
				return &InputError{Message: s.Name + ": host " + quote(h) + " is not a DNS name", Hint: "use a hostname such as shop.example.com"}
			}
		}
	}
	for i := range inv.Services {
		s := &inv.Services[i]
		if s.Public && len(s.Hosts) == 0 {
			if public == 1 && len(inv.Hostnames) > 0 {
				s.Hosts = append([]string(nil), inv.Hostnames...)
			} else {
				return &InputError{Message: s.Name + " is public but has no hosts", Hint: "set hosts, or set inventory hostnames when only one Service is public"}
			}
		}
		for _, dep := range s.DependsOn {
			if !names[dep] {
				return &InputError{Message: s.Name + " depends_on unknown service " + dep, Hint: "use a service name from this inventory"}
			}
			if dep == s.Name {
				return &InputError{Message: s.Name + " depends on itself", Hint: "remove that depends_on entry"}
			}
		}
	}
	return nil
}

func dropRailway(d *Result, svc string, env map[string]string, keys []string) (map[string]string, []string) {
	var dropped []string
	out := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "RAILWAY_") {
			dropped = append(dropped, k)
			continue
		}
		out[k] = v
	}
	var keep []string
	for _, k := range keys {
		if strings.HasPrefix(k, "RAILWAY_") {
			dropped = append(dropped, k)
			continue
		}
		keep = append(keep, k)
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		d.Warnings = append(d.Warnings, svc+": dropped Railway-provided "+strings.Join(dropped, ", ")+"; Yoho does not inject RAILWAY_*")
	}
	return out, keep
}

func renderCompose(source string, inv inventory, warnings []string) string {
	var b strings.Builder
	b.WriteString("# compose.yaml — drafted by `yoho migrate draft` from " + source + ".\n")
	b.WriteString("# Review before use. Nothing here is deployed by the draft.\n")
	b.WriteString("# Railway volumes are not Docker volumes on this Server. Postgres is filled with pg_restore.\n")
	for _, w := range warnings {
		b.WriteString("# " + w + "\n")
	}
	b.WriteString("services:\n")
	for _, s := range inv.Services {
		if isPostgres(s.Image) {
			b.WriteString("  # Starts empty. Restore the dump after the first deploy, before the cutover.\n")
		}
		b.WriteString("  " + s.Name + ":\n")
		writeService(&b, inv, s)
	}
	if len(inv.Services) > 0 && hasVolume(inv) {
		b.WriteString("volumes:\n")
		seen := map[string]bool{}
		for _, s := range inv.Services {
			if s.Volume == "" || seen[s.Volume] {
				continue
			}
			seen[s.Volume] = true
			b.WriteString("  " + s.Volume + ":\n")
		}
	}
	return b.String()
}

func writeService(b *strings.Builder, inv inventory, s service) {
	switch {
	case s.Dockerfile == "Dockerfile" || s.Dockerfile == "./Dockerfile":
		b.WriteString("    build: .\n")
	case s.Dockerfile != "":
		b.WriteString("    build:\n")
		b.WriteString("      context: .\n")
		b.WriteString("      dockerfile: " + scalar(s.Dockerfile) + "\n")
	case s.Image != "":
		b.WriteString("    image: " + scalar(s.Image) + "\n")
	default:
		b.WriteString("    # image: set an image or a Dockerfile before deploying\n")
	}
	if cmd := commandArgs(s); len(cmd) > 0 {
		b.WriteString("    command: " + flow(cmd) + "\n")
	}
	policy, _ := restartPolicy(s)
	b.WriteString("    restart: " + scalar(policy) + "\n")
	if len(s.Env) > 0 {
		b.WriteString("    environment:\n")
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("      " + k + ": " + scalar(s.Env[k]) + "\n")
		}
	}
	if s.Volume != "" {
		b.WriteString("    volumes:\n")
		b.WriteString("      - " + s.Volume + ":" + s.VolumeMount + "\n")
	}
	if len(s.DependsOn) > 0 {
		b.WriteString("    depends_on:\n")
		for _, dep := range s.DependsOn {
			cond := "service_started"
			target := serviceByName(inv, dep)
			if target.HealthcheckPath != "" || len(target.Healthcheck) > 0 || isPostgres(target.Image) {
				cond = "service_healthy"
			}
			b.WriteString("      " + dep + ":\n")
			b.WriteString("        condition: " + cond + "\n")
		}
	}
	writeHealth(b, s)
	var ext strings.Builder
	if s.Public {
		ext.WriteString("      proxy:\n")
		ext.WriteString("        hosts: " + flow(s.Hosts) + "\n")
		fmt.Fprintf(&ext, "        port: %d\n", s.Port)
		if s.HealthcheckPath != "" && s.HealthcheckPath != "/up" {
			ext.WriteString("        health_path: " + scalar(s.HealthcheckPath) + "\n")
		}
	}
	if len(s.SecretKeys) > 0 {
		ext.WriteString("      secrets:\n")
		for _, k := range s.SecretKeys {
			ext.WriteString("        - " + k + "\n")
		}
	}
	if rel := releaseArgs(s); len(rel) > 0 {
		ext.WriteString("      release_command: " + flow(rel) + "\n")
	}
	if s.Volume != "" || isPostgres(s.Image) {
		ext.WriteString("      stateful: true\n")
	}
	if isPostgres(s.Image) && !contains(s.SecretKeys, "POSTGRES_PASSWORD") {
		ext.WriteString("      generate:\n")
		ext.WriteString("        POSTGRES_PASSWORD: password32\n")
	}
	if s.Volume != "" {
		ext.WriteString("      backup:\n")
		if isPostgres(s.Image) {
			user := s.Env["POSTGRES_USER"]
			db := s.Env["POSTGRES_DB"]
			ext.WriteString("        dump: " + flow([]string{"pg_dump", "-U", user, db}) + "\n")
		}
		ext.WriteString("        volumes: [" + s.Volume + "]\n")
	}
	if ext.Len() > 0 {
		b.WriteString("    x-yoho:\n")
		b.WriteString(ext.String())
	}
}

func writeHealth(b *strings.Builder, s service) {
	switch {
	case len(s.Healthcheck) > 0:
		b.WriteString("    healthcheck:\n")
		b.WriteString("      test: " + flow(s.Healthcheck) + "\n")
	case s.HealthcheckPath != "" && s.Port > 0:
		b.WriteString("    healthcheck:\n")
		b.WriteString("      test: " + flow([]string{"CMD", "wget", "-qO-", fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, s.HealthcheckPath)}) + "\n")
	case isPostgres(s.Image):
		b.WriteString("    healthcheck:\n")
		b.WriteString("      test: " + flow([]string{"CMD-SHELL", "pg_isready -U " + s.Env["POSTGRES_USER"]}) + "\n")
	default:
		return
	}
	b.WriteString("      interval: 5s\n")
	b.WriteString("      timeout: 3s\n")
	b.WriteString("      retries: 20\n")
}

func renderYoho(source string, inv inventory, d *Result) (string, []string, []string) {
	var b strings.Builder
	b.WriteString("# yoho.yml — drafted by `yoho migrate draft` from " + source + ".\n")
	b.WriteString("# Switch configuration. On the first deploy, delete the migrate block so\n")
	b.WriteString("# ingress stays on Railway while you restore Postgres. A finished cutover\n")
	b.WriteString("# does not run again. from: compose is a label; Railway is not a migrate.from value.\n")
	b.WriteString("# stop names nothing on this Server, so the switch is:\n")
	b.WriteString("#   yoho migrate plan --cutover-allow-empty\n")
	b.WriteString("#   yoho deploy --skip-build --cutover-allow-empty\n")
	b.WriteString("# No claim: Railway volumes are not on this Server.\n")
	for _, w := range d.Warnings {
		b.WriteString("# " + w + "\n")
	}
	if len(d.Generated) > 0 {
		b.WriteString("# " + strings.Join(d.Generated, ", ") + " is generated on the Server. Do not copy Railway's value.\n")
	}
	b.WriteString("# yaml-language-server: $schema=https://yohobuild.com/schema/yoho.schema.json\n")
	b.WriteString("app: " + inv.Project + "\n\n")
	b.WriteString("servers:\n")
	b.WriteString("  primary:\n")
	b.WriteString("    ssh: root@203.0.113.10 # replace with root@YOUR_SERVER\n\n")
	b.WriteString("destinations:\n")
	b.WriteString("  production:\n")
	b.WriteString("    servers: [primary]\n")
	b.WriteString("    migrate:\n")
	b.WriteString("      from: compose\n")
	b.WriteString("      stop:\n")
	b.WriteString("        containers: [" + stopContainer(inv) + "]\n")
	if len(inv.Hostnames) > 0 {
		b.WriteString("      tunnel:\n")
		b.WriteString("        api_token_secret: CF_API_TOKEN\n")
		b.WriteString("        hostnames: " + flow(inv.Hostnames) + "\n")
	}
	b.WriteString("\nbuilder:\n")
	b.WriteString("  location: local\n\n")
	b.WriteString("proxy:\n")
	b.WriteString("  http_port: 0\n")
	b.WriteString("  https_port: 0\n")
	if len(inv.Hostnames) > 0 {
		b.WriteString("  tunnel:\n")
		b.WriteString("    token_secret: CF_TUNNEL_TOKEN\n")
	}
	b.WriteString("\nsetup:\n")
	b.WriteString("  user: yoho\n")

	var lines []string
	seen := map[string]bool{}
	for _, s := range inv.Services {
		for _, k := range s.SecretKeys {
			if isPostgres(s.Image) && k == "POSTGRES_PASSWORD" && contains(d.Generated, "POSTGRES_PASSWORD") {
				continue
			}
			if seen[k] || contains(d.Generated, k) {
				continue
			}
			seen[k] = true
			lines = append(lines, k+"=$(op read op://Production/"+inv.Project+"/"+k+")")
		}
	}
	if len(inv.Hostnames) > 0 {
		for _, k := range []string{"CF_API_TOKEN", "CF_TUNNEL_TOKEN"} {
			if !seen[k] {
				seen[k] = true
				lines = append(lines, k+"=$(op read op://Production/"+inv.Project+"/"+k+")")
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// SecretLines follow the compose declaration order, not the sorted keys.
	if len(lines) > 0 {
		b.WriteString("\n# Copy into .yoho/secrets. These are references, not values.\n")
		b.WriteString("# Bitwarden alternative: KEY=$(bw get password item)\n")
		for _, line := range lines {
			b.WriteString("# " + line + "\n")
		}
	}
	return b.String(), lines, keys
}

func stopContainer(inv inventory) string {
	name := inv.Services[0].Name
	for _, s := range inv.Services {
		if s.Public {
			name = s.Name
			break
		}
	}
	return "railway-" + inv.Project + "-" + name
}

func commandArgs(s service) []string {
	if len(s.Command) > 0 {
		return s.Command
	}
	if s.StartCommand == "" {
		return nil
	}
	return []string{"sh", "-c", s.StartCommand}
}

func releaseArgs(s service) []string {
	if len(s.ReleaseCommand) > 0 {
		return s.ReleaseCommand
	}
	if s.PreDeployCommand == "" {
		return nil
	}
	return []string{"sh", "-c", s.PreDeployCommand}
}

func restartPolicy(s service) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s.Restart)) {
	case "", "unless-stopped":
		return "unless-stopped", nil
	case "always":
		return "always", nil
	case "on_failure", "on-failure":
		return "on-failure", nil
	case "never", "no":
		return "no", nil
	default:
		return "", &InputError{Message: s.Name + ": restart " + quote(s.Restart) + " is not a compose policy or a Railway restartPolicyType", Hint: "use unless-stopped, always, on-failure, or no; Railway's names ALWAYS, ON_FAILURE, and NEVER are accepted"}
	}
}

func serviceByName(inv inventory, name string) service {
	for _, s := range inv.Services {
		if s.Name == name {
			return s
		}
	}
	return service{}
}

func hasVolume(inv inventory) bool {
	for _, s := range inv.Services {
		if s.Volume != "" {
			return true
		}
	}
	return false
}

func isPostgres(image string) bool {
	ref := image
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	name, _, _ := strings.Cut(ref, "@")
	name, _, _ = strings.Cut(name, ":")
	return name == "postgres"
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func ident(key, value string) error {
	if keyPattern.MatchString(value) {
		return nil
	}
	return &InputError{Message: key + " must be letters, digits, and underscores", Hint: "the draft puts it in a pg_isready and pg_dump command"}
}

func noNewlines(what, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return &InputError{Message: what + " must be one line", Hint: "split argv in command or release_command"}
	}
	return nil
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	return scalar(s)
}

func scalar(s string) string {
	var n yaml.Node
	if err := n.Encode(s); err != nil {
		return fmt.Sprintf("%q", s)
	}
	out, err := yaml.Marshal(&n)
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSpace(string(out))
}

func flow(ss []string) string {
	// Quote every element. A plain scalar with a space is legal as a block
	// value and illegal inside a flow sequence.
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
