# Cloudreve v4 - Codebase Conventions

> **Start here first:** [`docs/PROJECT_STATUS.md`](docs/PROJECT_STATUS.md) has
> current project state, what's in progress, and where things left off.
> Update it when a session ends with meaningful progress.

## General Rules
- NEVER rewrite existing code unless explicitly asked
- Match existing code style exactly - read nearby files before writing new code
- Do not add comments unless asked
- Do not add new dependencies without checking if an equivalent already exists in go.mod or package.json
- When adding features, follow the existing layer structure: router -> controller -> service -> inventory -> ent

## Build System
- No Makefile. Build uses GoReleaser (`.goreleaser.yaml`)
- Frontend: Vite + React + TypeScript in `assets/` (yarn build -> zip into application/statics/assets.zip)
- Ent ORM: `go generate ./...` regenerates from `ent/schema/`
- CLI uses Cobra: `cmd/server.go` is the default entry point
- Version injection via ldflags: `constants.BackendVersion`, `constants.LastCommit`

---

## Go Backend Conventions

### Architecture Layers
```
cmd/            CLI commands (Cobra)
application/    Server lifecycle, DI container (Dep), constants
routers/        Gin route definitions (router.go)
middleware/     Gin middleware (auth.go, session.go, etc.)
controllers/    Thin HTTP handlers (parse -> call service -> respond)
service/        Business logic with request validation (binding tags)
inventory/      Repository pattern over Ent ORM (interface + private impl)
ent/            Auto-generated ORM (schema definitions in ent/schema/)
pkg/            Reusable libraries (auth, cache, serializer, etc.)
```

### Dependency Injection
- Always retrieve deps via `dep := dependency.FromContext(c)` at the start of every handler/service method
- Never construct dependencies manually
- Access sub-clients: `dep.UserClient()`, `dep.FileClient()`, `dep.KV()`, `dep.SettingProvider()`, `dep.Logger()`, `dep.HashIDEncoder()`

### Controller Pattern
- Always HTTP 200 status code. Business errors go in the JSON body `code` field
- Parse request via generic middleware: `ParametersFromContext[*service.SomeService](c, service.SomeCtx{})`
- Call service method, handle error, return response
```go
func SomeHandler(c *gin.Context) {
    service := ParametersFromContext[*someservice.SomeService](c, someservice.SomeParameterCtx{})
    resp, err := service.DoSomething(c)
    if err != nil {
        c.JSON(200, serializer.Err(c, err))
        c.Abort()
        return
    }
    c.JSON(200, serializer.Response{Data: resp})
}
```
- Empty success: `c.JSON(200, serializer.Response{})`

### Service Pattern
- Service struct with binding tags for request validation
- Context key struct: `type SomeParameterCtx struct{}`
- Methods take `*gin.Context`, return `(result, error)` or just `error`
- Retrieve deps at method start: `dep := dependency.FromContext(c)`
- Create errors via: `serializer.NewError(serializer.CodeXxx, "message", err)`
- Free functions (no struct) for simple operations that don't need request binding

### Inventory (Repository) Pattern
- Interface: `type FooClient interface { ... }` with `TxOperator` embedded
- Private impl: `type fooClient struct { client *ent.Client }`
- Constructor: `func NewFooClient(client *ent.Client) FooClient`
- Eager loading via context flags: `ctx = context.WithValue(ctx, LoadFooBar{}, true)`
- Package-level error vars: `var ErrSomething = errors.New("...")`
- Transactions: `inventory.WithTx(ctx, client)`

### Error Handling
- App-level: `serializer.NewError(serializer.CodeXxx, "User-facing message", rawErr)`
- Internal: `fmt.Errorf("failed to do X: %w", err)`
- Error codes: integer constants in `pkg/serializer/error.go`
- Response: `serializer.Err(c, err)` or `serializer.ErrWithDetails(c, code, msg, err)`

### Import Grouping
```go
import (
    // stdlib
    "context"
    "fmt"

    // internal (github.com/cloudreve/...)
    "github.com/cloudreve/Cloudreve/v4/pkg/serializer"

    // third-party
    "github.com/gin-gonic/gin"
    "github.com/samber/lo"
)
```

### Naming
- Receiver: single lowercase letter (`c *userClient`, `f *fileClient`)
- Variables: short (`c`, `u`, `m`, `l`, `dep`, `s`, `err`)
- Service structs: `FooService`, `{Verb}{Noun}Service`
- Context keys: empty structs `type FooCtx struct{}`
- Import aliases when conflicts: `adminsvc "...service/admin"`
- Comments: Chinese common, English in newer code. Sparse - only on exported symbols when needed

---

## React Frontend Conventions

### Component Style
- Functional components ONLY - no class components
- Named exports preferred, default exports for some components
- Props interface: `ComponentNameProps` extending MUI props when applicable
```tsx
interface FooProps extends BoxProps {
  someProp: string;
}
const Foo = ({ someProp, ...rest }: FooProps) => { ... };
export default Foo;
```

### Styling
- MUI v6 components exclusively
- Reusable styled components in `component/Common/StyledComponents.tsx` using `styled()` API
- One-off styles via MUI `sx` prop
- NO raw CSS files unless copying existing pattern
- Theme-aware: use `theme.palette.*`, `theme.spacing.*`, `theme.typography.*`

### State Management
- Redux Toolkit with `configureStore`
- Typed hooks: `useAppDispatch`, `useAppSelector` (from `redux/hooks.ts`)
- API calls as Redux thunks: `export function someApi(): ThunkResponse<T>`
- Three slices: `siteConfig`, `globalState`, `fileManager`
- Thunks in `redux/thunks/`, slice definitions in `redux/*Slice.ts`

### API Calls
- All HTTP via axios wrapper in `api/request.ts`
- `send(url, config, opts)` is the core function
- API functions return `ThunkResponse<T>` and dispatch `send()`
- Response type: `Response<T>` with `{ data, code, msg, error, correlation_id }`
- Error class: `AppError` with i18n message lookup

### Routing
- React Router v6 with `createBrowserRouter`
- Lazy loading for heavy routes: `async lazy() { ... }`
- `Outlet` pattern for nested layouts

### i18n
- `useTranslation()` hook in components
- `i18n.t()` outside components
- Namespaces: `common`, `application` (default), `dashboard`
- All user-facing strings MUST use translation keys

### File Organization
```
src/
  api/              API functions + types (request.ts is the axios wrapper)
  component/
    Common/         Reusable UI primitives (StyledComponents, TimeBadge, etc.)
    Admin/          Admin panel
    FileManager/    File browsing
    Pages/          Top-level pages
    Viewers/        File viewers
  hooks/            Custom React hooks (useThing.ts)
  redux/
    store.ts        configureStore
    hooks.ts        typed useAppDispatch/useAppSelector
    *Slice.ts       Redux slices
    thunks/         Async thunks
  router/           Route definitions
  session/          Session/auth manager
  util/             Utility functions
  constants/        Constants
```

### Import Patterns
- Relative imports with explicit `.ts`/`.tsx` extensions
- Named imports preferred for project files
- No path aliases (all relative from file location)
```tsx
import { User } from "../../../api/user.ts";
import { useAppDispatch } from "../../../redux/hooks.ts";
```

### Naming
- Component files: `PascalCase.tsx`
- Hook files: `useThing.ts` or `useThing.tsx`
- Slice files: `*Slice.ts`
- Utility files: lowercase or camelCase
- Types: PascalCase interfaces/types

### TypeScript
- `interface` for object shapes (dominant pattern)
- `PayloadAction<T>` for Redux reducer actions
- `ThunkResponse<T>` for API function return types
- Inline types for simple props
