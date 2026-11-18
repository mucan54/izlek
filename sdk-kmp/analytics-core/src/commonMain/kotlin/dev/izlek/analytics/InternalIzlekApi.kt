package dev.izlek.analytics

/**
 * Marks declarations that are shared between the `analytics-*` modules but are not part of the
 * public SDK surface. They can change without notice.
 */
@RequiresOptIn(
    level = RequiresOptIn.Level.ERROR,
    message = "Internal izlek SDK API shared between analytics-* modules. Not for application use.",
)
@Retention(AnnotationRetention.BINARY)
@Target(
    AnnotationTarget.CLASS,
    AnnotationTarget.FUNCTION,
    AnnotationTarget.PROPERTY,
    AnnotationTarget.CONSTRUCTOR,
    AnnotationTarget.TYPEALIAS,
)
public annotation class InternalIzlekApi
