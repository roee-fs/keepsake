import { createFileRoute } from '@tanstack/react-router'
import { TenantSwitcher } from '../components/TenantSwitcher'

export const Route = createFileRoute('/')({
  component: Index,
})

// Placeholder for the overview screen (Task 9); wires up the switcher meanwhile.
function Index() {
  return (
    <div className="flex flex-col gap-4 p-4">
      <TenantSwitcher />
      <div>keepsake</div>
    </div>
  )
}
