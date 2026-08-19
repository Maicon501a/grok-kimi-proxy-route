package profile

import "errors"

var (
	// ErrUnknownProfile is returned when an explicit profile id is not found.
	ErrUnknownProfile = errors.New("profile: profile_id desconhecido")
	// ErrProfileUnavailable is returned when a known profile has no live CDP endpoint.
	ErrProfileUnavailable = errors.New("profile: endpoint CDP indisponivel para o perfil")
	// ErrNoProfiles is returned when no profiles are configured.
	ErrNoProfiles = errors.New("profile: nenhum perfil AI Studio configurado")
)
