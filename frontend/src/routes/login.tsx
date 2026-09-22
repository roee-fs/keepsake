import { useMutation } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { loginSessionPost } from '../client'
import { safeRedirectTarget } from '../lib/session'

export const Route = createFileRoute('/login')({
  validateSearch: (search: Record<string, unknown>): { redirect?: string } => ({
    redirect: typeof search.redirect === 'string' ? search.redirect : undefined,
  }),
  component: LoginPage,
})

function LoginPage() {
  const { redirect } = Route.useSearch()
  const [password, setPassword] = useState('')

  const mutation = useMutation({
    mutationFn: (password: string) => loginSessionPost({ body: { password }, throwOnError: true }),
    onSuccess: () => {
      // Full navigation, not router.navigate: the destination is an arbitrary
      // path captured before login, not one of this component's known routes.
      window.location.assign(safeRedirectTarget(redirect))
    },
  })

  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <form
        className="flex w-80 flex-col gap-3 rounded-md border border-line bg-surface p-6"
        onSubmit={(event) => {
          event.preventDefault()
          mutation.mutate(password)
        }}
      >
        <div className="mb-1">
          <div className="font-mono text-sm font-medium text-fg-faint">keepsake</div>
          <h1 className="text-lg font-semibold tracking-tight">Admin console</h1>
        </div>
        <input
          type="password"
          autoFocus
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          placeholder="Password"
          className="rounded-md border border-line bg-base px-3 py-2 text-fg placeholder:text-fg-faint"
        />
        <button
          type="submit"
          disabled={mutation.isPending || password === ''}
          className="rounded-md bg-accent px-3 py-2 font-medium text-fg transition-opacity hover:opacity-90 disabled:opacity-40"
        >
          Log in
        </button>
        {mutation.isError && <p className="text-sm text-warn">Incorrect password.</p>}
      </form>
    </div>
  )
}
