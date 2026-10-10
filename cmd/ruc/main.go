// Command ruc downloads, indexes and serves the Paraguayan taxpayer registry (DNIT).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jmendozaf/rucpy/internal/api"
	"github.com/jmendozaf/rucpy/internal/dnit"
	"github.com/jmendozaf/rucpy/internal/ruc"
	"github.com/jmendozaf/rucpy/internal/store"
	"github.com/jmendozaf/rucpy/internal/syncer"
)

var version = "dev"

const usage = `ruc: padrón de RUC de la DNIT (Paraguay)

Uso:
  ruc sync     [--db ruc.db] [--force]             descarga el padrón y actualiza la base
  ruc get      <ruc> [--db ruc.db]                 busca un RUC (80012345-6, 80012345 o código viejo)
  ruc check    <ruc>                               valida el dígito verificador (sin conexión)
  ruc search   <nombre> [--limit 20] [--db]        busca por nombre o razón social
  ruc changes  [--since 2026-10-01] [--status X]   cambios de estado entre sincronizaciones
  ruc stats    [--db ruc.db]                       totales y fecha del padrón
  ruc serve    [--addr :8080] [--sync-every 24h] [--cors https://sitio]   API HTTP
  ruc version

La base por defecto es $RUC_DB o ./ruc.db.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "sync":
		err = runSync(ctx, args)
	case "get":
		err = runGet(ctx, args)
	case "check":
		err = runCheck(args)
	case "search":
		err = runSearch(ctx, args)
	case "changes":
		err = runChanges(ctx, args)
	case "stats":
		err = runStats(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "version", "--version":
		fmt.Println("ruc", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ruc: comando desconocido %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	var exit exitError
	switch {
	case errors.As(err, &exit):
		os.Exit(int(exit))
	case err != nil:
		fmt.Fprintln(os.Stderr, "ruc:", err)
		os.Exit(1)
	}
}

// exitError ends the program with a status code and no extra message.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func defaultDB() string {
	if p := os.Getenv("RUC_DB"); p != "" {
		return p
	}
	return "ruc.db"
}

// parse lets flags appear before or after positional arguments ("ruc get 123 --db x" and "ruc get --db x 123").
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !isBoolFlag(fs, a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return positional, nil
}

func isBoolFlag(fs *flag.FlagSet, arg string) bool {
	f := fs.Lookup(strings.TrimLeft(arg, "-"))
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func openStore(path string) (*store.Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no existe %s; corré primero: ruc sync", path)
	}
	return store.Open(path)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	force := fs.Bool("force", false, "descargar todo aunque no haya cambios")
	workers := fs.Int("workers", 4, "descargas en paralelo")
	page := fs.String("source", dnit.DefaultPageURL, "página del listado de RUC")
	quiet := fs.Bool("quiet", false, "sin progreso")
	if _, err := parse(fs, args); err != nil {
		return err
	}

	client := dnit.NewClient()
	client.PageURL = *page
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	if *quiet {
		logf = nil
	}

	res, err := syncer.Run(ctx, client, *db, syncer.Options{Workers: *workers, Force: *force, Logf: logf})
	if err != nil {
		return err
	}
	if res.UpToDate {
		fmt.Fprintf(os.Stderr, "al día: %d RUC, la DNIT no publicó cambios\n", res.Rows)
		return nil
	}
	fmt.Fprintf(os.Stderr, "listo: %d RUC (%d descargados, %d sin cambios), %d cambios de estado, %s\n",
		res.Rows, res.Downloaded, res.Reused, res.Changes, res.Duration)
	if res.BadDV > 0 {
		fmt.Fprintf(os.Stderr, "aviso: %d filas con dígito verificador que no coincide\n", res.BadDV)
	}
	return nil
}

func runGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	rest, err := parse(fs, args)
	if err != nil || len(rest) != 1 {
		return errors.New("uso: ruc get <ruc>")
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()

	base := rest[0]
	if parsed, err := ruc.Parse(rest[0]); err == nil {
		if parsed.HasDV && !parsed.Valid() {
			fmt.Fprintf(os.Stderr, "dígito verificador incorrecto: debería ser %s\n", parsed.Base+"-"+fmt.Sprint(parsed.ExpectedDV()))
			return exitError(1)
		}
		base = parsed.Base
	}
	t, err := st.Get(ctx, base)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintln(os.Stderr, "no está en el padrón de la DNIT")
		return exitError(3)
	}
	if err != nil {
		return err
	}
	return printJSON(t)
}

func runCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("uso: ruc check <ruc>")
	}
	parsed, err := ruc.Parse(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "formato inválido")
		return exitError(1)
	}
	if !parsed.HasDV {
		fmt.Printf("%s (dígito verificador calculado)\n", parsed)
		return nil
	}
	if !parsed.Valid() {
		fmt.Printf("inválido: el dígito verificador de %s es %d\n", parsed.Base, parsed.ExpectedDV())
		return exitError(1)
	}
	fmt.Printf("válido: %s\n", parsed)
	return nil
}

func runSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	limit := fs.Int("limit", 20, "máximo de resultados")
	rest, err := parse(fs, args)
	if err != nil || len(rest) == 0 {
		return errors.New("uso: ruc search <nombre>")
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	results, err := st.Search(ctx, strings.Join(rest, " "), *limit)
	if err != nil {
		return err
	}
	return printJSON(results)
}

func runChanges(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("changes", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	since := fs.String("since", time.Now().AddDate(0, 0, -7).Format(time.DateOnly), "desde (YYYY-MM-DD)")
	status := fs.String("status", "", "solo cambios hacia este estado (ej: CANCELADO)")
	limit := fs.Int("limit", 100, "máximo de resultados")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	from, err := time.Parse(time.DateOnly, *since)
	if err != nil {
		return errors.New("--since debe ser YYYY-MM-DD")
	}

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	changes, err := st.Changes(ctx, from, strings.ToUpper(*status), *limit)
	if err != nil {
		return err
	}
	return printJSON(changes)
}

func runStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	return printJSON(stats)
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", defaultDB(), "archivo de base de datos")
	addr := fs.String("addr", envOr("RUC_ADDR", ":8080"), "dirección HTTP")
	every := fs.Duration("sync-every", envDuration("RUC_SYNC_EVERY", 0), "sincronizar cada (0 = nunca), ej: 24h")
	cors := fs.String("cors", os.Getenv("RUC_CORS_ORIGINS"), "sitios que pueden consultar la API desde el navegador, separados por coma (* = cualquiera)")
	if _, err := parse(fs, args); err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	srv := api.New(*db, log)
	defer srv.Close()
	for _, origin := range strings.Split(*cors, ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			srv.AllowedOrigins = append(srv.AllowedOrigins, origin)
		}
	}

	if *every > 0 {
		go keepSynced(ctx, *db, *every, srv, log)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", *addr, "db", *db, "sync_every", every.String())
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// keepSynced syncs right away if there is no database yet, then every interval, reloading the server after each run.
func keepSynced(ctx context.Context, db string, every time.Duration, srv *api.Server, log *slog.Logger) {
	run := func() {
		log.Info("sync started")
		res, err := syncer.Run(ctx, dnit.NewClient(), db, syncer.Options{})
		if err != nil {
			log.Error("sync failed", "err", err)
			return
		}
		if res.UpToDate {
			log.Info("sync: registry unchanged", "rows", res.Rows)
			return
		}
		if err := srv.Reload(); err != nil {
			log.Error("reload failed", "err", err)
			return
		}
		log.Info("sync finished", "rows", res.Rows, "downloaded", res.Downloaded, "reused", res.Reused,
			"changes", res.Changes, "duration", res.Duration.String())
	}

	if _, err := os.Stat(db); err != nil {
		run()
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return fallback
}
