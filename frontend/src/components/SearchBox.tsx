import { useEffect, useState } from 'react'

type SearchBoxProps = {
  value: string
  onChange: (query: string) => void
}

/**
 * One input drives three endpoints: empty browses, a leading `/` greps by
 * regex, anything else searches. Debounced locally so typing doesn't fire a
 * request (and a URL update) per keystroke.
 */
export function SearchBox({ value, onChange }: SearchBoxProps) {
  const [text, setText] = useState(value)

  // The URL can change from outside (back/forward, a prefix cleared elsewhere), so
  // follow it. Adjusting during render rather than in an effect avoids an extra
  // commit on every prop change.
  const [prevValue, setPrevValue] = useState(value)
  if (value !== prevValue) {
    setPrevValue(value)
    setText(value)
  }

  useEffect(() => {
    const id = setTimeout(() => {
      if (text !== value) onChange(text)
    }, 300)
    return () => clearTimeout(id)
  }, [text, value, onChange])

  return (
    <input
      type="text"
      value={text}
      onChange={(event) => setText(event.target.value)}
      placeholder="Search, or /pattern to grep"
      className="w-full rounded-md border border-line bg-surface px-3 py-1.5 text-fg placeholder:text-fg-faint"
    />
  )
}
