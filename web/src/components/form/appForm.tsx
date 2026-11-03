import { Button, Checkbox, NumberInput, Select, TagsInput, Textarea, TextInput, type ButtonProps, type CheckboxProps, type NumberInputProps, type SelectProps,
  type TagsInputProps, type TextareaProps, type TextInputProps } from '@mantine/core'
import { createFormHook, createFormHookContexts } from '@tanstack/react-form'
import type { ReactNode } from 'react'
import { fieldError } from '@/lib/form'

// Mantine inputs bound to TanStack Form fields (SM-ATT-007). A field component reads its field from context, so a form
// writes `<form.AppField name="name">{(f) => <f.TextField label="Name" />}</form.AppField>`; labels, descriptions and
// the other Mantine props pass through unchanged, and validation errors show once the field was touched.

export const { fieldContext, formContext, useFieldContext, useFormContext } = createFormHookContexts()

type Omitted = 'value' | 'defaultValue' | 'onChange' | 'error'

function TextField(props: Omit<TextInputProps, Omitted>) {
  const f = useFieldContext<string>()
  return <TextInput {...props} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
}

function TextareaField(props: Omit<TextareaProps, Omitted>) {
  const f = useFieldContext<string>()
  return <Textarea {...props} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />
}

/** A number; an emptied input falls back to `fallback` (the field never holds a non-number). */
function NumberField({ fallback = 0, ...props }: Omit<NumberInputProps, Omitted> & { fallback?: number }) {
  const f = useFieldContext<number>()
  return <NumberInput {...props} value={f.state.value} onBlur={f.handleBlur} onChange={(n) => f.handleChange(typeof n === 'number' ? n : Number(n) || fallback)}
    error={fieldError(f.state.meta)} />
}

/** A single choice stored as a string; clearing it stores the empty string. */
function SelectField(props: Omit<SelectProps, Omitted>) {
  const f = useFieldContext<string>()
  return <Select {...props} value={f.state.value || null} onBlur={f.handleBlur} onChange={(x) => f.handleChange(x ?? '')} error={fieldError(f.state.meta)} />
}

function TagsField(props: Omit<TagsInputProps, Omitted>) {
  const f = useFieldContext<string[]>()
  return <TagsInput {...props} value={f.state.value} onBlur={f.handleBlur} onChange={(x) => f.handleChange(x)} error={fieldError(f.state.meta)} />
}

function CheckboxField(props: Omit<CheckboxProps, Omitted | 'checked'>) {
  const f = useFieldContext<boolean>()
  return <Checkbox {...props} checked={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.currentTarget.checked)} />
}

/** The form's submit button: shows progress while submitting. */
function SubmitButton({ children, ...props }: ButtonProps & { children: ReactNode; disabled?: boolean }) {
  const form = useFormContext()
  return <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy} {...props}>{children}</Button>}</form.Subscribe>
}

export const { useAppForm } = createFormHook({
  fieldContext,
  formContext,
  fieldComponents: { TextField, TextareaField, NumberField, SelectField, TagsField, CheckboxField },
  formComponents: { SubmitButton },
})
