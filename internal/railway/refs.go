package railway

import (
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	referencePattern       = regexp.MustCompile(`\$\{\{\s*([A-Za-z0-9_ -]+)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	singleReferencePattern = regexp.MustCompile(`\$\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	inventoryEnvPath       = regexp.MustCompile(`^services\[[0-9]+\]\.env\.[A-Za-z_][A-Za-z0-9_]*$`)
	literalPassword        = regexp.MustCompile(`:[^/\s:@]+@`)
)

func inventoryEnvRef(value, at string, allowRefs bool) bool {
	return allowRefs && inventoryEnvPath.MatchString(at) && strings.Contains(value, "${{")
}

func resolveTarget(inv *inventory, name string) *service {
	name = strings.ReplaceAll(strings.ToLower(name), " ", "-")
	for i := range inv.Services {
		if strings.ReplaceAll(strings.ToLower(inv.Services[i].Name), " ", "-") == name {
			return &inv.Services[i]
		}
	}
	if name != "postgres" && name != "postgresql" {
		return nil
	}
	var target *service
	for i := range inv.Services {
		if isPostgres(inv.Services[i].Image) {
			if target != nil {
				return nil
			}
			target = &inv.Services[i]
		}
	}
	return target
}

// resolveRefs reads a snapshot so a referenced env value never becomes a literal
// merely because its Service happened to be visited earlier.
func resolveRefs(inv *inventory) (warnings []string, composed map[string]string, err error) {
	original := inventory{Services: append([]service(nil), inv.Services...)}
	for i := range original.Services {
		original.Services[i].Env = maps.Clone(original.Services[i].Env)
	}
	composed = map[string]string{}
	owners := map[string]string{}
	for i := range inv.Services {
		s := &inv.Services[i]
		keys := make([]string, 0, len(s.Env))
		for k := range s.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			value := s.Env[k]
			if !strings.Contains(value, "${{") {
				continue
			}
			if !keyPattern.MatchString(k) {
				return nil, nil, &InputError{Message: s.Name + ": env key is not a shell name", Hint: "use names like DATABASE_URL"}
			}
			if err := noNewlines(s.Name+" env."+k, value); err != nil {
				return nil, nil, err
			}
			unresolved := func(ref string) error {
				return &InputError{
					Message: s.Name + ": env." + k + " references " + ref + ", which cannot be resolved offline",
					Hint:    "add " + k + " to secret_keys and write " + k + "=$(op read op://Production/" + inv.Project + "/" + k + ") in .yoho/secrets",
				}
			}
			// Malformed references are never echoed: they can contain literal secrets.
			if strings.Contains(referencePattern.ReplaceAllString(value, ""), "${{") {
				if single := singleReferencePattern.FindStringSubmatch(value); len(single) > 0 {
					return nil, nil, unresolved("${{" + single[1] + "}}")
				}
				return nil, nil, unresolved("${{ }}")
			}
			secret := false
			var dotenv strings.Builder
			cursor := 0
			var refErr error
			resolved := referencePattern.ReplaceAllStringFunc(value, func(ref string) string {
				start := cursor + strings.Index(value[cursor:], ref)
				dotenv.WriteString(escapeDotenvLiteral(value[cursor:start]))
				cursor = start + len(ref)
				parts := referencePattern.FindStringSubmatch(ref)
				name, variable := strings.TrimSpace(parts[1]), parts[2]
				target := resolveTarget(&original, name)
				if target != nil && !strings.EqualFold(name, "shared") {
					if replacement, bearing, ok := referenceValue(target, variable); ok {
						if bearing {
							secret = true
							for j := range inv.Services {
								t := &inv.Services[j]
								if t.Name == target.Name && !contains(t.SecretKeys, "POSTGRES_PASSWORD") {
									t.SecretKeys = append(t.SecretKeys, "POSTGRES_PASSWORD")
								}
							}
						}
						if bearing {
							dotenv.WriteString(replacement)
						} else {
							dotenv.WriteString(escapeDotenvLiteral(replacement))
						}
						return replacement
					}
				}
				refErr = unresolved("${{" + name + "." + variable + "}}")
				return ""
			})
			if refErr != nil {
				return nil, nil, refErr
			}
			if !secret {
				s.Env[k] = resolved
				continue
			}
			if k == "POSTGRES_PASSWORD" && resolved != "${POSTGRES_PASSWORD}" {
				return nil, nil, &InputError{Message: s.Name + ": env.POSTGRES_PASSWORD conflicts with the composed secret's password dependency", Hint: "rename the consuming env key"}
			}
			dotenv.WriteString(escapeDotenvLiteral(value[cursor:]))
			// Quote only when dotenv would interpret literal text. Generated
			// password expansions remain active; inventory dollars stay literal.
			if dotenv.String() != resolved || strings.ContainsAny(resolved, " #\t'\"") {
				resolved = "\"" + dotenv.String() + "\""
			}
			if previous, ok := composed[k]; ok && previous != resolved {
				return nil, nil, &InputError{Message: "env." + k + " resolves differently in " + owners[k] + " and " + s.Name + "; rename one", Hint: "give the consuming env keys different names"}
			}
			composed[k], owners[k] = resolved, s.Name
			delete(s.Env, k)
			if !contains(s.SecretKeys, k) {
				s.SecretKeys = append(s.SecretKeys, k)
			}
			warnings = append(warnings, s.Name+": env."+k+" is composed in .yoho/secrets from POSTGRES_PASSWORD; POSTGRES_PASSWORD is resolved locally instead of generated on the Server")
		}
	}
	return warnings, composed, nil
}

func referenceValue(target *service, variable string) (value string, secret, ok bool) {
	switch variable {
	case "RAILWAY_PRIVATE_DOMAIN":
		return target.Name, false, true
	case "PORT":
		if target.Port > 0 {
			return strconv.Itoa(target.Port), false, true
		}
		if isPostgres(target.Image) {
			return "5432", false, true
		}
		return "", false, false
	}
	if strings.HasPrefix(variable, "RAILWAY_") {
		return "", false, false
	}
	if isPostgres(target.Image) {
		switch variable {
		case "PGHOST":
			return target.Name, false, true
		case "PGPORT":
			return "5432", false, true
		case "PGUSER", "POSTGRES_USER":
			value = target.Env["POSTGRES_USER"]
		case "PGDATABASE", "POSTGRES_DB":
			value = target.Env["POSTGRES_DB"]
		case "PGPASSWORD", "POSTGRES_PASSWORD":
			return "${POSTGRES_PASSWORD}", true, true
		case "DATABASE_URL", "DATABASE_PRIVATE_URL":
			user, db := target.Env["POSTGRES_USER"], target.Env["POSTGRES_DB"]
			if ident("POSTGRES_USER", user) != nil || ident("POSTGRES_DB", db) != nil {
				return "", false, false
			}
			return "postgres://" + user + ":${POSTGRES_PASSWORD}@" + target.Name + ":5432/" + db, true, true
		}
	}
	if value == "" {
		value, ok = target.Env[variable]
	} else {
		ok = true
	}
	return value, false, ok && !strings.Contains(value, "${{")
}

// escapeDotenvLiteral preserves inventory text without turning it into local
// command substitution or interpolation when a composed secret is loaded.
func escapeDotenvLiteral(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(value)
}
