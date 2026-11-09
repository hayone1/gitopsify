// Carbon Design System colors (the palettes are in assets/css/main.css).

// Text on a 10% tint of its own color (soft and subtle badges and alerts)
// is one step darker in light mode, for 4.5:1 (Carbon's tags do the same).
// The classes are spelled out for Tailwind to find them.
type Tinted = 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error'
const tint = (color: Tinted, cls: string) => ({ color, variant: ['soft', 'subtle'] as ('soft' | 'subtle')[], class: cls })
const tinted = [
  tint('primary', 'text-primary-600 dark:text-primary'),
  tint('secondary', 'text-secondary-600 dark:text-secondary'),
  tint('success', 'text-success-600 dark:text-success'),
  tint('info', 'text-info-600 dark:text-info'),
  tint('warning', 'text-warning-600 dark:text-warning'),
  tint('error', 'text-error-600 dark:text-error')
]

export default defineAppConfig({
  ui: {
    colors: {
      primary: 'carbon-blue',
      secondary: 'carbon-purple',
      success: 'carbon-green',
      info: 'carbon-blue',
      warning: 'carbon-yellow',
      error: 'carbon-red',
      neutral: 'carbon-gray'
    },
    button: {
      compoundVariants: [{
        // Carbon's primary button: Blue 60 with white text in both themes
        // (Nuxt UI's would turn light blue with dark text in dark mode).
        color: 'primary',
        variant: 'solid',
        class: 'text-white bg-(--cds-button-primary) hover:bg-(--cds-button-primary-hover) active:bg-(--cds-button-primary-active) disabled:bg-(--cds-button-primary) aria-disabled:bg-(--cds-button-primary)'
      }]
    },
    badge: { compoundVariants: [...tinted] },
    alert: { compoundVariants: [...tinted] },
    switch: {
      // Carbon's toggle-off track (Gray 50 / Gray 60): 3:1 against the page.
      slots: { base: 'data-[state=unchecked]:bg-(--ui-border-accented)' }
    },
    navigationMenu: {
      compoundVariants: [{
        // Carbon's side nav: the current page is selected (Gray 20 / Gray 80)
        // with a strip in the interactive color (Blue 60 / Blue 50) at its
        // start edge; its text stays neutral.
        orientation: 'vertical',
        variant: 'pill',
        active: true,
        class: {
          link: 'text-highlighted before:bg-accented after:absolute after:inset-y-px after:start-0 after:w-[3px] after:bg-primary',
          linkLeadingIcon: 'text-highlighted group-data-[state=open]:text-highlighted'
        }
      }]
    }
  }
})
