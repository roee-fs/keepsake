import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { isExternal, resolveLink } from '../lib/links'

export function BodyLink({
  href,
  title,
  children,
  source,
  tenant,
}: {
  href?: string
  title?: string
  children?: ReactNode
  source: string
  tenant: string
}) {
  if (href?.startsWith('#') || (href && isExternal(href))) {
    return (
      <a href={href} title={title} className="text-link hover:underline">
        {children}
      </a>
    )
  }
  const target = href ? resolveLink(href, source) : null
  if (!target) return <span title={title}>{children}</span>
  return (
    <Link to="/concepts/$" params={{ _splat: target }} search={{ tenant }} title={title} className="text-link hover:underline">
      {children}
    </Link>
  )
}
