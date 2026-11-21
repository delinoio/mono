---

title: Extract Default Non-primitive Parameter Value to Constant When Forwarding It to Memoized Children
impact: MEDIUM
impactDescription: keeps memoized children from re-rendering by using a constant for default value
tags: rerender, memo, optimization

---

## Extract Default Non-primitive Parameter Value to Constant When Forwarding It to Memoized Children

`memo()` compares only the props that the parent passes in (with `Object.is`), and it does this before the component function runs. A destructuring default such as `onClick = () => {}` is evaluated later, inside the render, so it cannot make `memo(UserAvatar)` re-render when the parent passes the same props. It does create a new function on every render of `UserAvatar`. That new function breaks memoization only where `UserAvatar` passes it on: as a prop to a memoized child, or in a hook dependency array.

To address this issue, extract the default value into a constant. The child then receives the same function on every render.

**Incorrect (the default `onClick` is a new function on every render of `UserAvatar`, so the memoized `Icon` re-renders):**

```tsx
const Icon = memo(function Icon({ onClick }: { onClick: () => void }) {
  // ...
})

const UserAvatar = memo(function UserAvatar({
  onClick = () => {},
  size,
}: {
  onClick?: () => void
  size?: number
}) {
  return <Icon onClick={onClick} />
})

// Used without optional onClick. Rerendering the parent with unchanged props skips UserAvatar.
// When another prop such as `size` changes, UserAvatar renders and Icon re-renders too.
<UserAvatar size={32} />
```

**Correct (stable default value):**

```tsx
const NOOP = () => {};

const UserAvatar = memo(function UserAvatar({
  onClick = NOOP,
  size,
}: {
  onClick?: () => void
  size?: number
}) {
  return <Icon onClick={onClick} />
})

// When `size` changes, UserAvatar renders but Icon skips: it still gets NOOP.
// A supplied onClick is forwarded unchanged: <UserAvatar onClick={handleClick} />
<UserAvatar size={32} />
```
